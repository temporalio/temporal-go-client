// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package internal

// All code in this file is private to the package.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/opentracing/opentracing-go"
	"github.com/uber-go/tally"
	commandpb "go.temporal.io/api/command/v1"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	querypb "go.temporal.io/api/query/v1"
	"go.temporal.io/api/serviceerror"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	"go.temporal.io/api/workflowservice/v1"

	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/internal/common"
	"go.temporal.io/sdk/internal/common/backoff"
	"go.temporal.io/sdk/internal/common/cache"
	"go.temporal.io/sdk/internal/common/metrics"
	"go.temporal.io/sdk/internal/common/util"
	"go.temporal.io/sdk/log"
)

const (
	defaultHeartBeatInterval = 10 * 60 * time.Second

	defaultStickyCacheSize = 10000

	noRetryBackoff = time.Duration(-1)
)

type (
	// workflowExecutionEventHandler process a single event.
	workflowExecutionEventHandler interface {
		// Process a single event and return the assosciated commands.
		// Return List of commands made, any error.
		ProcessEvent(event *historypb.HistoryEvent, isReplay bool, isLast bool) error
		// ProcessQuery process a query request.
		ProcessQuery(queryType string, queryArgs *commonpb.Payloads) (*commonpb.Payloads, error)
		StackTrace() string
		// Close for cleaning up resources on this event handler
		Close()
	}

	// workflowTask wraps a workflow task.
	workflowTask struct {
		task            *workflowservice.PollWorkflowTaskQueueResponse
		historyIterator HistoryIterator
		doneCh          chan struct{}
		laResultCh      chan *localActivityResult
	}

	// activityTask wraps a activity task.
	activityTask struct {
		task          *workflowservice.PollActivityTaskQueueResponse
		pollStartTime time.Time
	}

	// resetStickinessTask wraps a ResetStickyTaskQueueRequest.
	resetStickinessTask struct {
		task *workflowservice.ResetStickyTaskQueueRequest
	}

	// workflowExecutionContextImpl is the cached workflow state for sticky execution
	workflowExecutionContextImpl struct {
		mutex             sync.Mutex
		workflowStartTime time.Time
		workflowInfo      *WorkflowInfo
		wth               *workflowTaskHandlerImpl

		// eventHandler is changed to a atomic.Value as a temporally bug fix for local activity
		// retry issue (github issue #915). Therefore, when accessing/modifying this field, the
		// mutex should still be held.
		eventHandler atomic.Value

		isWorkflowCompleted bool
		result              *commonpb.Payloads
		err                 error

		previousStartedEventID int64

		newCommands         []*commandpb.Command
		currentWorkflowTask *workflowservice.PollWorkflowTaskQueueResponse
		laTunnel            *localActivityTunnel
	}

	// workflowTaskHandlerImpl is the implementation of WorkflowTaskHandler
	workflowTaskHandlerImpl struct {
		namespace              string
		metricsScope           tally.Scope
		ppMgr                  pressurePointMgr
		logger                 log.Logger
		identity               string
		enableLoggingInReplay  bool
		disableStickyExecution bool
		registry               *registry
		laTunnel               *localActivityTunnel
		workflowPanicPolicy    WorkflowPanicPolicy
		dataConverter          converter.DataConverter
		contextPropagators     []ContextPropagator
		tracer                 opentracing.Tracer
	}

	activityProvider func(name string) activity

	// activityTaskHandlerImpl is the implementation of ActivityTaskHandler
	activityTaskHandlerImpl struct {
		taskQueueName      string
		identity           string
		service            workflowservice.WorkflowServiceClient
		metricsScope       tally.Scope
		logger             log.Logger
		userContext        context.Context
		registry           *registry
		activityProvider   activityProvider
		dataConverter      converter.DataConverter
		workerStopCh       <-chan struct{}
		contextPropagators []ContextPropagator
		tracer             opentracing.Tracer
	}

	// history wrapper method to help information about events.
	history struct {
		workflowTask   *workflowTask
		eventsHandler  *workflowExecutionEventHandlerImpl
		loadedEvents   []*historypb.HistoryEvent
		currentIndex   int
		nextEventID    int64 // next expected eventID for sanity
		lastEventID    int64 // last expected eventID, zero indicates read until end of stream
		next           []*historypb.HistoryEvent
		binaryChecksum string
	}

	workflowTaskHeartbeatError struct {
		Message string
	}
)

func newHistory(task *workflowTask, eventsHandler *workflowExecutionEventHandlerImpl) *history {
	result := &history{
		workflowTask:  task,
		eventsHandler: eventsHandler,
		loadedEvents:  task.task.History.Events,
		currentIndex:  0,
		lastEventID:   task.task.GetStartedEventId(),
	}
	if len(result.loadedEvents) > 0 {
		result.nextEventID = result.loadedEvents[0].GetEventId()
	}
	return result
}

func (e workflowTaskHeartbeatError) Error() string {
	return e.Message
}

// Get workflow start event.
func (eh *history) GetWorkflowStartedEvent() (*historypb.HistoryEvent, error) {
	events := eh.workflowTask.task.History.Events
	if len(events) == 0 || events[0].GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED {
		return nil, errors.New("unable to find WorkflowExecutionStartedEventAttributes in the history")
	}
	return events[0], nil
}

func (eh *history) IsReplayEvent(event *historypb.HistoryEvent) bool {
	return event.GetEventId() <= eh.workflowTask.task.GetPreviousStartedEventId() || isCommandEvent(event.GetEventType())
}

func (eh *history) IsNextWorkflowTaskFailed() (isFailed bool, binaryChecksum string, err error) {

	nextIndex := eh.currentIndex + 1
	if nextIndex >= len(eh.loadedEvents) && eh.hasMoreEvents() { // current page ends and there is more pages
		if err := eh.loadMoreEvents(); err != nil {
			return false, "", err
		}
	}

	if nextIndex < len(eh.loadedEvents) {
		nextEvent := eh.loadedEvents[nextIndex]
		nextEventType := nextEvent.GetEventType()
		isFailed := nextEventType == enumspb.EVENT_TYPE_WORKFLOW_TASK_TIMED_OUT || nextEventType == enumspb.EVENT_TYPE_WORKFLOW_TASK_FAILED
		var binaryChecksum string
		if nextEventType == enumspb.EVENT_TYPE_WORKFLOW_TASK_COMPLETED {
			binaryChecksum = nextEvent.GetWorkflowTaskCompletedEventAttributes().BinaryChecksum
		}
		return isFailed, binaryChecksum, nil
	}
	return false, "", nil
}

func (eh *history) loadMoreEvents() error {
	historyPage, err := eh.getMoreEvents()
	if err != nil {
		return err
	}
	eh.loadedEvents = append(eh.loadedEvents, historyPage.Events...)
	if eh.nextEventID == 0 && len(eh.loadedEvents) > 0 {
		eh.nextEventID = eh.loadedEvents[0].GetEventId()
	}
	return nil
}

func isCommandEvent(eventType enumspb.EventType) bool {
	switch eventType {
	case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED,
		enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_FAILED,
		enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CANCELED,
		enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CONTINUED_AS_NEW,
		enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED,
		enumspb.EVENT_TYPE_ACTIVITY_TASK_CANCEL_REQUESTED,
		enumspb.EVENT_TYPE_TIMER_STARTED,
		enumspb.EVENT_TYPE_TIMER_CANCELED,
		enumspb.EVENT_TYPE_MARKER_RECORDED,
		enumspb.EVENT_TYPE_START_CHILD_WORKFLOW_EXECUTION_INITIATED,
		enumspb.EVENT_TYPE_REQUEST_CANCEL_EXTERNAL_WORKFLOW_EXECUTION_INITIATED,
		enumspb.EVENT_TYPE_SIGNAL_EXTERNAL_WORKFLOW_EXECUTION_INITIATED,
		enumspb.EVENT_TYPE_UPSERT_WORKFLOW_SEARCH_ATTRIBUTES:
		return true
	default:
		return false
	}
}

// NextCommandEvents returns events that there processed as new by the next command.
// TODO(maxim): Refactor to return a struct instead of multiple parameters
func (eh *history) NextCommandEvents() (result []*historypb.HistoryEvent, markers []*historypb.HistoryEvent, binaryChecksum string, err error) {
	if eh.next == nil {
		eh.next, _, err = eh.nextCommandEvents()
		if err != nil {
			return result, markers, eh.binaryChecksum, err
		}
	}

	result = eh.next
	checksum := eh.binaryChecksum
	if len(result) > 0 {
		eh.next, markers, err = eh.nextCommandEvents()
	}
	return result, markers, checksum, err
}

func (eh *history) hasMoreEvents() bool {
	historyIterator := eh.workflowTask.historyIterator
	return historyIterator != nil && historyIterator.HasNextPage()
}

func (eh *history) getMoreEvents() (*historypb.History, error) {
	return eh.workflowTask.historyIterator.GetNextPage()
}

func (eh *history) verifyAllEventsProcessed() error {
	if eh.lastEventID > 0 && eh.nextEventID <= eh.lastEventID {
		return fmt.Errorf(
			"history_events: premature end of stream, expectedLastEventID=%v but no more events after eventID=%v",
			eh.lastEventID,
			eh.nextEventID-1)
	}
	if eh.lastEventID > 0 && eh.nextEventID != (eh.lastEventID+1) {
		eh.eventsHandler.logger.Warn(
			"history_events: processed events past the expected lastEventID",
			"expectedLastEventID", eh.lastEventID,
			"processedLastEventID", eh.nextEventID-1)
	}
	return nil
}

func (eh *history) nextCommandEvents() (nextEvents []*historypb.HistoryEvent, markers []*historypb.HistoryEvent, err error) {
	if eh.currentIndex == len(eh.loadedEvents) && !eh.hasMoreEvents() {
		if err := eh.verifyAllEventsProcessed(); err != nil {
			return nil, nil, err
		}
		return []*historypb.HistoryEvent{}, []*historypb.HistoryEvent{}, nil
	}

	// Process events

OrderEvents:
	for {
		// load more history events if needed
		for eh.currentIndex == len(eh.loadedEvents) {
			if !eh.hasMoreEvents() {
				if err = eh.verifyAllEventsProcessed(); err != nil {
					return
				}
				break OrderEvents
			}
			if err = eh.loadMoreEvents(); err != nil {
				return
			}
		}

		event := eh.loadedEvents[eh.currentIndex]
		eventID := event.GetEventId()
		if eventID != eh.nextEventID {
			err = fmt.Errorf(
				"missing history events, expectedNextEventID=%v but receivedNextEventID=%v",
				eh.nextEventID, eventID)
			return
		}

		eh.nextEventID++

		switch event.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED:
			isFailed, binaryChecksum, err1 := eh.IsNextWorkflowTaskFailed()
			if err1 != nil {
				err = err1
				return
			}
			if !isFailed {
				eh.binaryChecksum = binaryChecksum
				eh.currentIndex++
				nextEvents = append(nextEvents, event)
				break OrderEvents
			}
		case enumspb.EVENT_TYPE_WORKFLOW_TASK_SCHEDULED,
			enumspb.EVENT_TYPE_WORKFLOW_TASK_TIMED_OUT,
			enumspb.EVENT_TYPE_WORKFLOW_TASK_FAILED:
			// Skip
		default:
			if isPreloadMarkerEvent(event) {
				markers = append(markers, event)
			}
			nextEvents = append(nextEvents, event)
		}
		eh.currentIndex++
	}

	// shrink loaded events so it can be GCed
	eh.loadedEvents = append(
		make(
			[]*historypb.HistoryEvent,
			0,
			len(eh.loadedEvents)-eh.currentIndex),
		eh.loadedEvents[eh.currentIndex:]...,
	)

	eh.currentIndex = 0

	return nextEvents, markers, nil
}

func isPreloadMarkerEvent(event *historypb.HistoryEvent) bool {
	return event.GetEventType() == enumspb.EVENT_TYPE_MARKER_RECORDED
}

// newWorkflowTaskHandler returns an implementation of workflow task handler.
func newWorkflowTaskHandler(params workerExecutionParameters, ppMgr pressurePointMgr, registry *registry) WorkflowTaskHandler {
	ensureRequiredParams(&params)
	return &workflowTaskHandlerImpl{
		namespace:              params.Namespace,
		logger:                 params.Logger,
		ppMgr:                  ppMgr,
		metricsScope:           params.MetricsScope,
		identity:               params.Identity,
		enableLoggingInReplay:  params.EnableLoggingInReplay,
		disableStickyExecution: params.DisableStickyExecution,
		registry:               registry,
		workflowPanicPolicy:    params.WorkflowPanicPolicy,
		dataConverter:          params.DataConverter,
		contextPropagators:     params.ContextPropagators,
		tracer:                 params.Tracer,
	}
}

// TODO: need a better eviction policy based on memory usage
var workflowCache cache.Cache
var stickyCacheSize = defaultStickyCacheSize
var initCacheOnce sync.Once
var stickyCacheLock sync.Mutex

// SetStickyWorkflowCacheSize sets the cache size for sticky workflow cache. Sticky workflow execution is the affinity
// between workflow tasks of a specific workflow execution to a specific worker. The affinity is set if sticky execution
// is enabled via Worker.Options (It is enabled by default unless disabled explicitly). The benefit of sticky execution
// is that workflow does not have to reconstruct the state by replaying from beginning of history events. But the cost
// is it consumes more memory as it rely on caching workflow execution's running state on the worker. The cache is shared
// between workers running within same process. This must be called before any worker is started. If not called, the
// default size of 10K (might change in future) will be used.
func SetStickyWorkflowCacheSize(cacheSize int) {
	stickyCacheLock.Lock()
	defer stickyCacheLock.Unlock()
	if workflowCache != nil {
		panic("cache already created, please set cache size before worker starts.")
	}
	stickyCacheSize = cacheSize
}

func getWorkflowCache() cache.Cache {
	initCacheOnce.Do(func() {
		stickyCacheLock.Lock()
		defer stickyCacheLock.Unlock()
		workflowCache = cache.New(stickyCacheSize, &cache.Options{
			RemovedFunc: func(cachedEntity interface{}) {
				wc := cachedEntity.(*workflowExecutionContextImpl)
				wc.onEviction()
			},
		})
	})
	return workflowCache
}

func getWorkflowContext(runID string) *workflowExecutionContextImpl {
	o := getWorkflowCache().Get(runID)
	if o == nil {
		return nil
	}
	wc := o.(*workflowExecutionContextImpl)
	return wc
}

func putWorkflowContext(runID string, wc *workflowExecutionContextImpl) (*workflowExecutionContextImpl, error) {
	existing, err := getWorkflowCache().PutIfNotExist(runID, wc)
	if err != nil {
		return nil, err
	}
	return existing.(*workflowExecutionContextImpl), nil
}

func removeWorkflowContext(runID string) {
	getWorkflowCache().Delete(runID)
}

func newWorkflowExecutionContext(
	startTime time.Time,
	workflowInfo *WorkflowInfo,
	taskHandler *workflowTaskHandlerImpl,
) *workflowExecutionContextImpl {
	workflowContext := &workflowExecutionContextImpl{
		workflowStartTime: startTime,
		workflowInfo:      workflowInfo,
		wth:               taskHandler,
	}
	workflowContext.createEventHandler()
	return workflowContext
}

func (w *workflowExecutionContextImpl) Lock() {
	w.mutex.Lock()
}

func (w *workflowExecutionContextImpl) Unlock(err error) {
	if err != nil || w.err != nil || w.isWorkflowCompleted || (w.wth.disableStickyExecution && !w.hasPendingLocalActivityWork()) {
		// TODO: in case of closed, it asumes the close command always succeed. need server side change to return
		// error to indicate the close failure case. This should be rare case. For now, always remove the cache, and
		// if the close command failed, the next command will have to rebuild the state.
		if getWorkflowCache().Exist(w.workflowInfo.WorkflowExecution.RunID) {
			removeWorkflowContext(w.workflowInfo.WorkflowExecution.RunID)
		} else {
			// sticky is disabled, manually clear the workflow state.
			w.clearState()
		}
	}

	w.mutex.Unlock()
}

func (w *workflowExecutionContextImpl) getEventHandler() *workflowExecutionEventHandlerImpl {
	eventHandler := w.eventHandler.Load()
	if eventHandler == nil {
		return nil
	}
	eventHandlerImpl, ok := eventHandler.(*workflowExecutionEventHandlerImpl)
	if !ok {
		panic("unknown type for workflow execution event handler")
	}
	return eventHandlerImpl
}

func (w *workflowExecutionContextImpl) completeWorkflow(result *commonpb.Payloads, err error) {
	w.isWorkflowCompleted = true
	w.result = result
	w.err = err
}

func (w *workflowExecutionContextImpl) shouldResetStickyOnEviction() bool {
	// Not all evictions from the cache warrant a call to the server
	// to reset stickiness.
	// Cases when this is redundant or unnecessary include
	// when an error was encountered during execution
	// or workflow simply completed successfully.
	return w.err == nil && !w.isWorkflowCompleted
}

func (w *workflowExecutionContextImpl) onEviction() {
	// onEviction is run by LRU cache's removeFunc in separate goroutinue
	w.mutex.Lock()

	// Queue a ResetStickiness request *BEFORE* calling clearState
	// because once destroyed, no sensible information
	// may be ascertained about the execution context's state,
	// nor should any of its methods be invoked.
	if w.shouldResetStickyOnEviction() {
		w.queueResetStickinessTask()
	}

	w.clearState()
	w.mutex.Unlock()
}

func (w *workflowExecutionContextImpl) IsDestroyed() bool {
	return w.getEventHandler() == nil
}

func (w *workflowExecutionContextImpl) queueResetStickinessTask() {
	var task resetStickinessTask
	task.task = &workflowservice.ResetStickyTaskQueueRequest{
		Namespace: w.workflowInfo.Namespace,
		Execution: &commonpb.WorkflowExecution{
			WorkflowId: w.workflowInfo.WorkflowExecution.ID,
			RunId:      w.workflowInfo.WorkflowExecution.RunID,
		},
	}
	// w.laTunnel could be nil for worker.ReplayHistory() because there is no worker started, in that case we don't
	// care about resetStickinessTask.
	if w.laTunnel != nil && w.laTunnel.resultCh != nil {
		w.laTunnel.resultCh <- &task
	}
}

func (w *workflowExecutionContextImpl) clearState() {
	w.clearCurrentTask()
	w.isWorkflowCompleted = false
	w.result = nil
	w.err = nil
	w.previousStartedEventID = 0
	w.newCommands = nil

	eventHandler := w.getEventHandler()
	if eventHandler != nil {
		// Set isReplay to true to prevent user code in defer guarded by !isReplaying() from running
		eventHandler.isReplay = true
		eventHandler.Close()
		w.eventHandler.Store((*workflowExecutionEventHandlerImpl)(nil))
	}
}

func (w *workflowExecutionContextImpl) createEventHandler() {
	w.clearState()
	eventHandler := newWorkflowExecutionEventHandler(
		w.workflowInfo,
		w.completeWorkflow,
		w.wth.logger,
		w.wth.enableLoggingInReplay,
		w.wth.metricsScope,
		w.wth.registry,
		w.wth.dataConverter,
		w.wth.contextPropagators,
		w.wth.tracer,
	)
	w.eventHandler.Store(eventHandler)
}

func resetHistory(task *workflowservice.PollWorkflowTaskQueueResponse, historyIterator HistoryIterator) (*historypb.History, error) {
	historyIterator.Reset()
	firstPageHistory, err := historyIterator.GetNextPage()
	if err != nil {
		return nil, err
	}
	task.History = firstPageHistory
	return firstPageHistory, nil
}

func (wth *workflowTaskHandlerImpl) createWorkflowContext(task *workflowservice.PollWorkflowTaskQueueResponse) (*workflowExecutionContextImpl, error) {
	h := task.History
	attributes := h.Events[0].GetWorkflowExecutionStartedEventAttributes()
	if attributes == nil {
		return nil, errors.New("first history event is not WorkflowExecutionStarted")
	}
	taskQueue := attributes.TaskQueue
	if taskQueue == nil || taskQueue.Name == "" {
		return nil, errors.New("nil or empty TaskQueue in WorkflowExecutionStarted event")
	}

	runID := task.WorkflowExecution.GetRunId()
	workflowID := task.WorkflowExecution.GetWorkflowId()

	// Setup workflow Info
	var parentWorkflowExecution *WorkflowExecution
	if attributes.ParentWorkflowExecution != nil {
		parentWorkflowExecution = &WorkflowExecution{
			ID:    attributes.ParentWorkflowExecution.GetWorkflowId(),
			RunID: attributes.ParentWorkflowExecution.GetRunId(),
		}
	}
	workflowInfo := &WorkflowInfo{
		WorkflowExecution: WorkflowExecution{
			ID:    workflowID,
			RunID: runID,
		},
		WorkflowType:             WorkflowType{Name: task.WorkflowType.GetName()},
		TaskQueueName:            taskQueue.GetName(),
		WorkflowExecutionTimeout: common.DurationValue(attributes.GetWorkflowExecutionTimeout()),
		WorkflowRunTimeout:       common.DurationValue(attributes.GetWorkflowRunTimeout()),
		WorkflowTaskTimeout:      common.DurationValue(attributes.GetWorkflowTaskTimeout()),
		Namespace:                wth.namespace,
		Attempt:                  attributes.GetAttempt(),
		lastCompletionResult:     attributes.LastCompletionResult,
		lastFailure:              attributes.ContinuedFailure,
		CronSchedule:             attributes.CronSchedule,
		ContinuedExecutionRunID:  attributes.ContinuedExecutionRunId,
		ParentWorkflowNamespace:  attributes.ParentWorkflowNamespace,
		ParentWorkflowExecution:  parentWorkflowExecution,
		Memo:                     attributes.Memo,
		SearchAttributes:         attributes.SearchAttributes,
	}

	wfStartTime := common.TimeValue(h.Events[0].GetEventTime())
	return newWorkflowExecutionContext(wfStartTime, workflowInfo, wth), nil
}

func (wth *workflowTaskHandlerImpl) getOrCreateWorkflowContext(
	task *workflowservice.PollWorkflowTaskQueueResponse,
	historyIterator HistoryIterator,
) (workflowContext *workflowExecutionContextImpl, err error) {
	workflowMetricsScope := metrics.GetMetricsScopeForWorkflow(wth.metricsScope, task.WorkflowType.GetName())
	defer func() {
		if err == nil && workflowContext != nil && workflowContext.laTunnel == nil {
			workflowContext.laTunnel = wth.laTunnel
		}
		workflowMetricsScope.Gauge(metrics.StickyCacheSize).Update(float64(getWorkflowCache().Size()))
	}()

	runID := task.WorkflowExecution.GetRunId()

	history := task.History
	isFullHistory := isFullHistory(history)

	workflowContext = nil
	if task.Query == nil || (task.Query != nil && !isFullHistory) {
		workflowContext = getWorkflowContext(runID)
	}

	if workflowContext != nil {
		workflowContext.Lock()
		if task.Query != nil && !isFullHistory {
			// query task and we have a valid cached state
			workflowMetricsScope.Counter(metrics.StickyCacheHit).Inc(1)
		} else if history.Events[0].GetEventId() == workflowContext.previousStartedEventID+1 {
			// non query task and we have a valid cached state
			workflowMetricsScope.Counter(metrics.StickyCacheHit).Inc(1)
		} else {
			// non query task and cached state is missing events, we need to discard the cached state and rebuild one.
			_ = workflowContext.ResetIfStale(task, historyIterator)
		}
	} else {
		if !isFullHistory {
			// we are getting partial history task, but cached state was already evicted.
			// we need to reset history so we get events from beginning to replay/rebuild the state
			workflowMetricsScope.Counter(metrics.StickyCacheMiss).Inc(1)
			if _, err = resetHistory(task, historyIterator); err != nil {
				return
			}
		}

		if workflowContext, err = wth.createWorkflowContext(task); err != nil {
			return
		}

		if !wth.disableStickyExecution && task.Query == nil {
			workflowContext, _ = putWorkflowContext(runID, workflowContext)
		}
		workflowContext.Lock()
	}

	err = workflowContext.resetStateIfDestroyed(task, historyIterator)
	if err != nil {
		workflowContext.Unlock(err)
	}

	return
}

func isFullHistory(history *historypb.History) bool {
	if len(history.Events) == 0 || history.Events[0].GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED {
		return false
	}
	return true
}

func (w *workflowExecutionContextImpl) resetStateIfDestroyed(task *workflowservice.PollWorkflowTaskQueueResponse, historyIterator HistoryIterator) error {
	// It is possible that 2 threads (one for workflow task and one for query task) that both are getting this same
	// cached workflowContext. If one task finished with err, it would destroy the cached state. In that case, the
	// second task needs to reset the cache state and start from beginning of the history.
	if w.IsDestroyed() {
		w.createEventHandler()
		// reset history events if necessary
		if !isFullHistory(task.History) {
			if _, err := resetHistory(task, historyIterator); err != nil {
				return err
			}
		}
	}
	return nil
}

// ProcessWorkflowTask processes all the events of the workflow task.
func (wth *workflowTaskHandlerImpl) ProcessWorkflowTask(
	workflowTask *workflowTask,
	heartbeatFunc workflowTaskHeartbeatFunc,
) (completeRequest interface{}, errRet error) {
	if workflowTask == nil || workflowTask.task == nil {
		return nil, errors.New("nil workflow task provided")
	}
	task := workflowTask.task
	if task.History == nil || len(task.History.Events) == 0 {
		task.History = &historypb.History{
			Events: []*historypb.HistoryEvent{},
		}
	}
	if task.Query == nil && len(task.History.Events) == 0 {
		return nil, errors.New("nil or empty history")
	}

	if task.Query != nil && len(task.Queries) != 0 {
		return nil, errors.New("invalid query workflow task")
	}

	runID := task.WorkflowExecution.GetRunId()
	workflowID := task.WorkflowExecution.GetWorkflowId()
	traceLog(func() {
		wth.logger.Debug("Processing new workflow task.",
			tagWorkflowType, task.WorkflowType.GetName(),
			tagWorkflowID, workflowID,
			tagRunID, runID,
			"PreviousStartedEventId", task.GetPreviousStartedEventId())
	})

	workflowContext, err := wth.getOrCreateWorkflowContext(task, workflowTask.historyIterator)
	if err != nil {
		return nil, err
	}

	defer func() {
		workflowContext.Unlock(errRet)
	}()

	var response interface{}
processWorkflowLoop:
	for {
		startTime := time.Now()
		response, err = workflowContext.ProcessWorkflowTask(workflowTask)
		if err == nil && response == nil {
		waitLocalActivityLoop:
			for {
				deadlineToTrigger := time.Duration(float32(ratioToForceCompleteWorkflowTaskComplete) * float32(workflowContext.workflowInfo.WorkflowTaskTimeout))
				delayDuration := time.Until(startTime.Add(deadlineToTrigger))
				select {
				case <-time.After(delayDuration):
					// force complete, call the workflow task heartbeat function
					workflowTask, err = heartbeatFunc(
						workflowContext.CompleteWorkflowTask(workflowTask, false),
						startTime,
					)
					if err != nil {
						errRet = &workflowTaskHeartbeatError{Message: fmt.Sprintf("error sending workflow task heartbeat %v", err)}
						return
					}
					if workflowTask == nil {
						return
					}

					continue processWorkflowLoop

				case lar := <-workflowTask.laResultCh:
					// local activity result ready
					response, err = workflowContext.ProcessLocalActivityResult(workflowTask, lar)
					if err == nil && response == nil {
						// workflow task is not done yet, still waiting for more local activities
						continue waitLocalActivityLoop
					}
					break processWorkflowLoop
				}
			}
		} else {
			break processWorkflowLoop
		}
	}
	errRet = err
	completeRequest = response
	return
}

func (w *workflowExecutionContextImpl) ProcessWorkflowTask(workflowTask *workflowTask) (interface{}, error) {
	task := workflowTask.task
	historyIterator := workflowTask.historyIterator
	if err := w.ResetIfStale(task, historyIterator); err != nil {
		return nil, err
	}
	w.SetCurrentTask(task)

	eventHandler := w.getEventHandler()
	reorderedHistory := newHistory(workflowTask, eventHandler)
	var replayCommands []*commandpb.Command
	var respondEvents []*historypb.HistoryEvent

	skipReplayCheck := w.skipReplayCheck()
	workflowMetricsScope := metrics.GetMetricsScopeForWorkflow(w.wth.metricsScope, task.WorkflowType.GetName())
	replayStopWatch := workflowMetricsScope.Timer(metrics.WorkflowTaskReplayLatency).Start()
	replayStopWatchStopped := false

	// Process events
ProcessEvents:
	for {
		reorderedEvents, markers, binaryChecksum, err := reorderedHistory.NextCommandEvents()
		if err != nil {
			return nil, err
		}

		if len(reorderedEvents) == 0 {
			break ProcessEvents
		}
		if binaryChecksum == "" {
			w.workflowInfo.BinaryChecksum = getBinaryChecksum()
		} else {
			w.workflowInfo.BinaryChecksum = binaryChecksum
		}
		// Markers are from the events that are produced from the current workflow task.
		for _, m := range markers {
			if m.GetMarkerRecordedEventAttributes().GetMarkerName() != localActivityMarkerName {
				// local activity marker needs to be applied after workflow task started event
				err := eventHandler.ProcessEvent(m, true, false)
				if err != nil {
					return nil, err
				}
				if w.isWorkflowCompleted {
					break ProcessEvents
				}
			}
		}

		for i, event := range reorderedEvents {
			isInReplay := reorderedHistory.IsReplayEvent(event)
			if !isInReplay && !replayStopWatchStopped {
				replayStopWatch.Stop()
				replayStopWatchStopped = true
			}

			isLast := !isInReplay && i == len(reorderedEvents)-1
			if !skipReplayCheck && isCommandEvent(event.GetEventType()) {
				respondEvents = append(respondEvents, event)
			}

			if isPreloadMarkerEvent(event) {
				// marker events are processed separately
				continue
			}

			// Any pressure points.
			err := w.wth.executeAnyPressurePoints(event, isInReplay)
			if err != nil {
				return nil, err
			}

			err = eventHandler.ProcessEvent(event, isInReplay, isLast)
			if err != nil {
				return nil, err
			}
			if w.isWorkflowCompleted {
				break ProcessEvents
			}
		}

		// now apply local activity markers
		for _, m := range markers {
			if m.GetMarkerRecordedEventAttributes().GetMarkerName() == localActivityMarkerName {
				err := eventHandler.ProcessEvent(m, true, false)
				if err != nil {
					return nil, err
				}
				if w.isWorkflowCompleted {
					break ProcessEvents
				}
			}
		}
		isReplay := len(reorderedEvents) > 0 && reorderedHistory.IsReplayEvent(reorderedEvents[len(reorderedEvents)-1])
		if isReplay {
			eventCommands := eventHandler.commandsHelper.getCommands(true)
			if len(eventCommands) > 0 && !skipReplayCheck {
				replayCommands = append(replayCommands, eventCommands...)
			}
		}
	}

	if !replayStopWatchStopped {
		replayStopWatch.Stop()
	}

	// Non-deterministic error could happen in 2 different places:
	//   1) the replay commands does not match to history events. This is usually due to non backwards compatible code
	// change to workflow logic. For example, change calling one activity to a different activity.
	//   2) the command state machine is trying to make illegal state transition while replay a history event (like
	// activity task completed), but the corresponding workflow code that start the event has been removed. In that case
	// the replay of that event will panic on the command state machine and the workflow will be marked as completed
	// with the panic error.
	var workflowError error
	if !skipReplayCheck && !w.isWorkflowCompleted {
		// check if commands from reply matches to the history events
		if err := matchReplayWithHistory(replayCommands, respondEvents); err != nil {
			workflowError = err
		}
	}

	return w.applyWorkflowPanicPolicy(workflowTask, workflowError)
}

func (w *workflowExecutionContextImpl) ProcessLocalActivityResult(workflowTask *workflowTask, lar *localActivityResult) (interface{}, error) {
	if lar.err != nil && w.retryLocalActivity(lar) {
		return nil, nil // nothing to do here as we are retrying...
	}

	return w.applyWorkflowPanicPolicy(workflowTask, w.getEventHandler().ProcessLocalActivityResult(lar))
}

func (w *workflowExecutionContextImpl) applyWorkflowPanicPolicy(workflowTask *workflowTask, workflowError error) (interface{}, error) {
	task := workflowTask.task

	if workflowError == nil && w.err != nil {
		if panicErr, ok := w.err.(*workflowPanicError); ok {
			workflowError = panicErr
		}
	}

	if workflowError != nil {
		if panicErr, ok := w.err.(*workflowPanicError); ok {
			w.wth.logger.Error("Workflow panic",
				tagWorkflowType, task.WorkflowType.GetName(),
				tagWorkflowID, task.WorkflowExecution.GetWorkflowId(),
				tagRunID, task.WorkflowExecution.GetRunId(),
				tagError, workflowError,
				tagStackTrace, panicErr.StackTrace())
		} else {
			w.wth.logger.Error("Workflow panic",
				tagWorkflowType, task.WorkflowType.GetName(),
				tagWorkflowID, task.WorkflowExecution.GetWorkflowId(),
				tagRunID, task.WorkflowExecution.GetRunId(),
				tagError, workflowError)
		}

		switch w.wth.workflowPanicPolicy {
		case FailWorkflow:
			// complete workflow with custom error will fail the workflow
			w.getEventHandler().Complete(nil, NewApplicationError(
				"Workflow failed on panic due to FailWorkflow workflow panic policy",
				"", false, workflowError))
		case BlockWorkflow:
			// return error here will be convert to WorkflowTaskFailed for the first time, and ignored for subsequent
			// attempts which will cause WorkflowTaskTimeout and server will retry forever until issue got fixed or
			// workflow timeout.
			return nil, workflowError
		default:
			panic("unknown mismatched workflow history policy.")
		}
	}

	return w.CompleteWorkflowTask(workflowTask, true), nil
}

func (w *workflowExecutionContextImpl) retryLocalActivity(lar *localActivityResult) bool {
	if lar.task.retryPolicy == nil || lar.err == nil || IsCanceledError(lar.err) {
		return false
	}

	retryBackoff := getRetryBackoff(lar, time.Now(), w.wth.dataConverter)
	if retryBackoff > 0 && retryBackoff <= w.workflowInfo.WorkflowTaskTimeout {
		// we need a local retry
		time.AfterFunc(retryBackoff, func() {
			// TODO: this should not be a separate goroutine as it introduces race condition when accessing eventHandler.
			// currently this is solved by changing eventHandler to an atomic.Value. Ideally, this retry timer should be
			// part of the event loop for processing the workflow task.
			eventHandler := w.getEventHandler()

			// if workflow task heartbeat failed, the workflow execution context will be cleared and eventHandler will be nil
			if eventHandler == nil {
				return
			}

			if _, ok := eventHandler.pendingLaTasks[lar.task.activityID]; !ok {
				return
			}

			lar.task.attempt++

			if !w.laTunnel.sendTask(lar.task) {
				lar.task.attempt--
			}
		})
		return true
	}
	// Backoff could be large and potentially much larger than WorkflowTaskTimeout. We cannot just sleep locally for
	// retry. Because it will delay the local activity from complete which keeps the workflow task open. In order to
	// keep workflow task open, we have to keep "heartbeating" current workflow task.
	// In that case, it is more efficient to create a server timer with backoff duration and retry when that backoff
	// timer fires. So here we will return false to indicate we don't need local retry anymore. However, we have to
	// store the current attempt and backoff to the same LocalActivityResultMarker so the replay can do the right thing.
	// The backoff timer will be created by workflow.ExecuteLocalActivity().
	lar.backoff = retryBackoff

	return false
}

func getRetryBackoff(lar *localActivityResult, now time.Time, dataConverter converter.DataConverter) time.Duration {
	return getRetryBackoffWithNowTime(lar.task.retryPolicy, lar.task.attempt, lar.err, now, lar.task.expireTime)
}

func getRetryBackoffWithNowTime(p *RetryPolicy, attempt int32, err error, now, expireTime time.Time) time.Duration {
	if !IsRetryable(err, p.NonRetryableErrorTypes) {
		return noRetryBackoff
	}

	if p.MaximumAttempts > 0 && attempt > p.MaximumAttempts {
		return noRetryBackoff // max attempt reached
	}

	backoffInterval := time.Duration(float64(p.InitialInterval) * math.Pow(p.BackoffCoefficient, float64(attempt)))
	if backoffInterval <= 0 {
		// math.Pow() could overflow
		if p.MaximumInterval > 0 {
			backoffInterval = p.MaximumInterval
		} else {
			return noRetryBackoff
		}
	}

	if p.MaximumInterval > 0 && backoffInterval > p.MaximumInterval {
		// cap next interval to MaxInterval
		backoffInterval = p.MaximumInterval
	}

	nextScheduleTime := now.Add(backoffInterval)
	if !expireTime.IsZero() && nextScheduleTime.After(expireTime) {
		return noRetryBackoff
	}

	return backoffInterval
}

func (w *workflowExecutionContextImpl) CompleteWorkflowTask(workflowTask *workflowTask, waitLocalActivities bool) interface{} {
	if w.currentWorkflowTask == nil {
		return nil
	}
	eventHandler := w.getEventHandler()

	// w.laTunnel could be nil for worker.ReplayHistory() because there is no worker started, in that case we don't
	// care about the pending local activities, and just return because the result is ignored anyway by the caller.
	if w.hasPendingLocalActivityWork() && w.laTunnel != nil {
		if len(eventHandler.unstartedLaTasks) > 0 {
			// start new local activity tasks
			unstartedLaTasks := make(map[string]struct{})
			for activityID := range eventHandler.unstartedLaTasks {
				task := eventHandler.pendingLaTasks[activityID]
				task.wc = w
				task.workflowTask = workflowTask
				if !w.laTunnel.sendTask(task) {
					unstartedLaTasks[activityID] = struct{}{}
					task.wc = nil
					task.workflowTask = nil
				}
			}
			eventHandler.unstartedLaTasks = unstartedLaTasks
		}
		// cannot complete workflow task as there are pending local activities
		if waitLocalActivities {
			return nil
		}
	}

	eventCommands := eventHandler.commandsHelper.getCommands(true)
	if len(eventCommands) > 0 {
		w.newCommands = append(w.newCommands, eventCommands...)
	}

	completeRequest := w.wth.completeWorkflow(eventHandler, w.currentWorkflowTask, w, w.newCommands, !waitLocalActivities)
	w.clearCurrentTask()

	return completeRequest
}

func (w *workflowExecutionContextImpl) hasPendingLocalActivityWork() bool {
	eventHandler := w.getEventHandler()
	return !w.isWorkflowCompleted &&
		w.currentWorkflowTask != nil &&
		w.currentWorkflowTask.Query == nil && // don't run local activity for query task
		eventHandler != nil &&
		len(eventHandler.pendingLaTasks) > 0
}

func (w *workflowExecutionContextImpl) clearCurrentTask() {
	w.newCommands = nil
	w.currentWorkflowTask = nil
}

func (w *workflowExecutionContextImpl) skipReplayCheck() bool {
	return w.currentWorkflowTask.Query != nil || !isFullHistory(w.currentWorkflowTask.History)
}

func (w *workflowExecutionContextImpl) SetCurrentTask(task *workflowservice.PollWorkflowTaskQueueResponse) {
	w.currentWorkflowTask = task
	// do not update the previousStartedEventID for query task
	if task.Query == nil {
		w.previousStartedEventID = task.GetStartedEventId()
	}
}

func (w *workflowExecutionContextImpl) ResetIfStale(task *workflowservice.PollWorkflowTaskQueueResponse, historyIterator HistoryIterator) error {
	if len(task.History.Events) > 0 && task.History.Events[0].GetEventId() != w.previousStartedEventID+1 {
		w.wth.logger.Debug("Cached state staled, new task has unexpected events",
			tagWorkflowID, task.WorkflowExecution.GetWorkflowId(),
			tagRunID, task.WorkflowExecution.GetRunId(),
			"CachedPreviousStartedEventID", w.previousStartedEventID,
			"TaskFirstEventID", task.History.Events[0].GetEventId(),
			"TaskStartedEventID", task.GetStartedEventId(),
			"TaskPreviousStartedEventID", task.GetPreviousStartedEventId())

		w.clearState()
		return w.resetStateIfDestroyed(task, historyIterator)
	}
	return nil
}

func skipDeterministicCheckForCommand(d *commandpb.Command) bool {
	if d.GetCommandType() == enumspb.COMMAND_TYPE_RECORD_MARKER {
		markerName := d.GetRecordMarkerCommandAttributes().GetMarkerName()
		if markerName == versionMarkerName || markerName == mutableSideEffectMarkerName {
			return true
		}
	}
	return false
}

func skipDeterministicCheckForEvent(e *historypb.HistoryEvent) bool {
	if e.GetEventType() == enumspb.EVENT_TYPE_MARKER_RECORDED {
		markerName := e.GetMarkerRecordedEventAttributes().GetMarkerName()
		if markerName == versionMarkerName || markerName == mutableSideEffectMarkerName {
			return true
		}
	}
	return false
}

// special check for upsert change version event
func skipDeterministicCheckForUpsertChangeVersion(events []*historypb.HistoryEvent, idx int) bool {
	e := events[idx]
	if e.GetEventType() == enumspb.EVENT_TYPE_MARKER_RECORDED &&
		e.GetMarkerRecordedEventAttributes().GetMarkerName() == versionMarkerName &&
		idx < len(events)-1 &&
		events[idx+1].GetEventType() == enumspb.EVENT_TYPE_UPSERT_WORKFLOW_SEARCH_ATTRIBUTES {
		if _, ok := events[idx+1].GetUpsertWorkflowSearchAttributesEventAttributes().SearchAttributes.IndexedFields[TemporalChangeVersion]; ok {
			return true
		}
	}
	return false
}

func matchReplayWithHistory(replayCommands []*commandpb.Command, historyEvents []*historypb.HistoryEvent) error {
	di := 0
	hi := 0
	hSize := len(historyEvents)
	dSize := len(replayCommands)
matchLoop:
	for hi < hSize || di < dSize {
		var e *historypb.HistoryEvent
		if hi < hSize {
			e = historyEvents[hi]
			if skipDeterministicCheckForUpsertChangeVersion(historyEvents, hi) {
				hi += 2
				continue matchLoop
			}
			if skipDeterministicCheckForEvent(e) {
				hi++
				continue matchLoop
			}
		}

		var d *commandpb.Command
		if di < dSize {
			d = replayCommands[di]
			if skipDeterministicCheckForCommand(d) {
				di++
				continue matchLoop
			}
		}

		if d == nil {
			return fmt.Errorf("nondeterministic workflow: missing replay command for %s", util.HistoryEventToString(e))
		}

		if e == nil {
			return fmt.Errorf("nondeterministic workflow: extra replay command for %s", util.CommandToString(d))
		}

		if !isCommandMatchEvent(d, e, false) {
			return fmt.Errorf("nondeterministic workflow: history event is %s, replay command is %s",
				util.HistoryEventToString(e), util.CommandToString(d))
		}

		di++
		hi++
	}
	return nil
}

func lastPartOfName(name string) string {
	lastDotIdx := strings.LastIndex(name, ".")
	if lastDotIdx < 0 || lastDotIdx == len(name)-1 {
		return name
	}
	return name[lastDotIdx+1:]
}

func isCommandMatchEvent(d *commandpb.Command, e *historypb.HistoryEvent, strictMode bool) bool {
	switch d.GetCommandType() {
	case enumspb.COMMAND_TYPE_SCHEDULE_ACTIVITY_TASK:
		if e.GetEventType() != enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED {
			return false
		}
		eventAttributes := e.GetActivityTaskScheduledEventAttributes()
		commandAttributes := d.GetScheduleActivityTaskCommandAttributes()

		if eventAttributes.GetActivityId() != commandAttributes.GetActivityId() ||
			lastPartOfName(eventAttributes.ActivityType.GetName()) != lastPartOfName(commandAttributes.ActivityType.GetName()) ||
			(strictMode && eventAttributes.TaskQueue.GetName() != commandAttributes.TaskQueue.GetName()) ||
			(strictMode && !proto.Equal(eventAttributes.GetInput(), commandAttributes.GetInput())) {
			return false
		}

		return true

	case enumspb.COMMAND_TYPE_REQUEST_CANCEL_ACTIVITY_TASK:
		if e.GetEventType() != enumspb.EVENT_TYPE_ACTIVITY_TASK_CANCEL_REQUESTED {
			return false
		}
		commandAttributes := d.GetRequestCancelActivityTaskCommandAttributes()
		eventAttributes := e.GetActivityTaskCancelRequestedEventAttributes()
		if eventAttributes.GetScheduledEventId() != commandAttributes.GetScheduledEventId() {
			return false
		}

		return true

	case enumspb.COMMAND_TYPE_START_TIMER:
		if e.GetEventType() != enumspb.EVENT_TYPE_TIMER_STARTED {
			return false
		}
		eventAttributes := e.GetTimerStartedEventAttributes()
		commandAttributes := d.GetStartTimerCommandAttributes()

		if eventAttributes.GetTimerId() != commandAttributes.GetTimerId() ||
			(strictMode && common.DurationValue(eventAttributes.GetStartToFireTimeout()) != common.DurationValue(commandAttributes.GetStartToFireTimeout())) {
			return false
		}

		return true

	case enumspb.COMMAND_TYPE_CANCEL_TIMER:
		if e.GetEventType() != enumspb.EVENT_TYPE_TIMER_CANCELED {
			return false
		}
		commandAttributes := d.GetCancelTimerCommandAttributes()
		if e.GetEventType() == enumspb.EVENT_TYPE_TIMER_CANCELED {
			eventAttributes := e.GetTimerCanceledEventAttributes()
			if eventAttributes.GetTimerId() != commandAttributes.GetTimerId() {
				return false
			}
		}

		return true

	case enumspb.COMMAND_TYPE_COMPLETE_WORKFLOW_EXECUTION:
		if e.GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED {
			return false
		}
		if strictMode {
			eventAttributes := e.GetWorkflowExecutionCompletedEventAttributes()
			commandAttributes := d.GetCompleteWorkflowExecutionCommandAttributes()

			if !proto.Equal(eventAttributes.GetResult(), commandAttributes.GetResult()) {
				return false
			}
		}

		return true

	case enumspb.COMMAND_TYPE_FAIL_WORKFLOW_EXECUTION:
		if e.GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_FAILED {
			return false
		}
		if strictMode {
			eventAttributes := e.GetWorkflowExecutionFailedEventAttributes()
			commandAttributes := d.GetFailWorkflowExecutionCommandAttributes()

			if !proto.Equal(eventAttributes.GetFailure(), commandAttributes.GetFailure()) {
				return false
			}
		}

		return true

	case enumspb.COMMAND_TYPE_RECORD_MARKER:
		if e.GetEventType() != enumspb.EVENT_TYPE_MARKER_RECORDED {
			return false
		}
		eventAttributes := e.GetMarkerRecordedEventAttributes()
		commandAttributes := d.GetRecordMarkerCommandAttributes()
		if eventAttributes.GetMarkerName() != commandAttributes.GetMarkerName() {
			return false
		}

		return true

	case enumspb.COMMAND_TYPE_REQUEST_CANCEL_EXTERNAL_WORKFLOW_EXECUTION:
		if e.GetEventType() != enumspb.EVENT_TYPE_REQUEST_CANCEL_EXTERNAL_WORKFLOW_EXECUTION_INITIATED {
			return false
		}
		eventAttributes := e.GetRequestCancelExternalWorkflowExecutionInitiatedEventAttributes()
		commandAttributes := d.GetRequestCancelExternalWorkflowExecutionCommandAttributes()
		if checkNamespacesInCommandAndEvent(eventAttributes.GetNamespace(), commandAttributes.GetNamespace()) ||
			eventAttributes.WorkflowExecution.GetWorkflowId() != commandAttributes.GetWorkflowId() {
			return false
		}

		return true

	case enumspb.COMMAND_TYPE_SIGNAL_EXTERNAL_WORKFLOW_EXECUTION:
		if e.GetEventType() != enumspb.EVENT_TYPE_SIGNAL_EXTERNAL_WORKFLOW_EXECUTION_INITIATED {
			return false
		}
		eventAttributes := e.GetSignalExternalWorkflowExecutionInitiatedEventAttributes()
		commandAttributes := d.GetSignalExternalWorkflowExecutionCommandAttributes()
		if checkNamespacesInCommandAndEvent(eventAttributes.GetNamespace(), commandAttributes.GetNamespace()) ||
			eventAttributes.GetSignalName() != commandAttributes.GetSignalName() ||
			eventAttributes.WorkflowExecution.GetWorkflowId() != commandAttributes.Execution.GetWorkflowId() {
			return false
		}

		return true

	case enumspb.COMMAND_TYPE_CANCEL_WORKFLOW_EXECUTION:
		if e.GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CANCELED {
			return false
		}
		if strictMode {
			eventAttributes := e.GetWorkflowExecutionCanceledEventAttributes()
			commandAttributes := d.GetCancelWorkflowExecutionCommandAttributes()
			if !proto.Equal(eventAttributes.GetDetails(), commandAttributes.GetDetails()) {
				return false
			}
		}
		return true

	case enumspb.COMMAND_TYPE_CONTINUE_AS_NEW_WORKFLOW_EXECUTION:
		if e.GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CONTINUED_AS_NEW {
			return false
		}

		return true

	case enumspb.COMMAND_TYPE_START_CHILD_WORKFLOW_EXECUTION:
		if e.GetEventType() != enumspb.EVENT_TYPE_START_CHILD_WORKFLOW_EXECUTION_INITIATED {
			return false
		}
		eventAttributes := e.GetStartChildWorkflowExecutionInitiatedEventAttributes()
		commandAttributes := d.GetStartChildWorkflowExecutionCommandAttributes()
		if lastPartOfName(eventAttributes.WorkflowType.GetName()) != lastPartOfName(commandAttributes.WorkflowType.GetName()) ||
			(strictMode && checkNamespacesInCommandAndEvent(eventAttributes.GetNamespace(), commandAttributes.GetNamespace())) ||
			(strictMode && eventAttributes.TaskQueue.GetName() != commandAttributes.TaskQueue.GetName()) {
			return false
		}

		return true

	case enumspb.COMMAND_TYPE_UPSERT_WORKFLOW_SEARCH_ATTRIBUTES:
		if e.GetEventType() != enumspb.EVENT_TYPE_UPSERT_WORKFLOW_SEARCH_ATTRIBUTES {
			return false
		}
		eventAttributes := e.GetUpsertWorkflowSearchAttributesEventAttributes()
		commandAttributes := d.GetUpsertWorkflowSearchAttributesCommandAttributes()
		if strictMode && !isSearchAttributesMatched(eventAttributes.SearchAttributes, commandAttributes.SearchAttributes) {
			return false
		}
		return true
	}

	return false
}

func isSearchAttributesMatched(attrFromEvent, attrFromCommand *commonpb.SearchAttributes) bool {
	if attrFromEvent != nil && attrFromCommand != nil {
		return reflect.DeepEqual(attrFromEvent.IndexedFields, attrFromCommand.IndexedFields)
	}
	return attrFromEvent == nil && attrFromCommand == nil
}

// return true if the check fails:
//    namespace is not empty in command
//    and namespace is not replayNamespace
//    and namespaces unmatch in command and events
func checkNamespacesInCommandAndEvent(eventNamespace, commandNamespace string) bool {
	if commandNamespace == "" || IsReplayNamespace(commandNamespace) {
		return false
	}
	return eventNamespace != commandNamespace
}

func (wth *workflowTaskHandlerImpl) completeWorkflow(
	eventHandler *workflowExecutionEventHandlerImpl,
	task *workflowservice.PollWorkflowTaskQueueResponse,
	workflowContext *workflowExecutionContextImpl,
	commands []*commandpb.Command,
	forceNewWorkflowTask bool) interface{} {

	// for query task
	if task.Query != nil {
		queryCompletedRequest := &workflowservice.RespondQueryTaskCompletedRequest{TaskToken: task.TaskToken}
		var panicErr *PanicError
		if errors.As(workflowContext.err, &panicErr) {
			queryCompletedRequest.CompletedType = enumspb.QUERY_RESULT_TYPE_FAILED
			queryCompletedRequest.ErrorMessage = "Workflow panic: " + panicErr.Error()
			return queryCompletedRequest
		}

		result, err := eventHandler.ProcessQuery(task.Query.GetQueryType(), task.Query.QueryArgs)
		if err != nil {
			queryCompletedRequest.CompletedType = enumspb.QUERY_RESULT_TYPE_FAILED
			queryCompletedRequest.ErrorMessage = err.Error()
		} else {
			queryCompletedRequest.CompletedType = enumspb.QUERY_RESULT_TYPE_ANSWERED
			queryCompletedRequest.QueryResult = result
		}
		return queryCompletedRequest
	}

	metricsScope := metrics.GetMetricsScopeForWorkflow(wth.metricsScope, eventHandler.workflowEnvironmentImpl.workflowInfo.WorkflowType.Name)

	// complete workflow task
	var closeCommand *commandpb.Command
	var canceledErr *CanceledError
	var contErr *ContinueAsNewError

	if errors.As(workflowContext.err, &canceledErr) {
		// Workflow canceled
		metricsScope.Counter(metrics.WorkflowCanceledCounter).Inc(1)
		closeCommand = createNewCommand(enumspb.COMMAND_TYPE_CANCEL_WORKFLOW_EXECUTION)
		closeCommand.Attributes = &commandpb.Command_CancelWorkflowExecutionCommandAttributes{CancelWorkflowExecutionCommandAttributes: &commandpb.CancelWorkflowExecutionCommandAttributes{
			Details: convertErrDetailsToPayloads(canceledErr.details, wth.dataConverter),
		}}
	} else if errors.As(workflowContext.err, &contErr) {
		// Continue as new error.
		metricsScope.Counter(metrics.WorkflowContinueAsNewCounter).Inc(1)
		closeCommand = createNewCommand(enumspb.COMMAND_TYPE_CONTINUE_AS_NEW_WORKFLOW_EXECUTION)
		closeCommand.Attributes = &commandpb.Command_ContinueAsNewWorkflowExecutionCommandAttributes{ContinueAsNewWorkflowExecutionCommandAttributes: &commandpb.ContinueAsNewWorkflowExecutionCommandAttributes{
			WorkflowType:        &commonpb.WorkflowType{Name: contErr.params.WorkflowType.Name},
			Input:               contErr.params.Input,
			TaskQueue:           &taskqueuepb.TaskQueue{Name: contErr.params.TaskQueueName, Kind: enumspb.TASK_QUEUE_KIND_NORMAL},
			WorkflowRunTimeout:  &contErr.params.WorkflowRunTimeout,
			WorkflowTaskTimeout: &contErr.params.WorkflowTaskTimeout,
			Header:              contErr.params.Header,
			Memo:                workflowContext.workflowInfo.Memo,
			SearchAttributes:    workflowContext.workflowInfo.SearchAttributes,
		}}
	} else if workflowContext.err != nil {
		// Workflow failures
		metricsScope.Counter(metrics.WorkflowFailedCounter).Inc(1)
		closeCommand = createNewCommand(enumspb.COMMAND_TYPE_FAIL_WORKFLOW_EXECUTION)
		failure := convertErrorToFailure(workflowContext.err, wth.dataConverter)
		closeCommand.Attributes = &commandpb.Command_FailWorkflowExecutionCommandAttributes{FailWorkflowExecutionCommandAttributes: &commandpb.FailWorkflowExecutionCommandAttributes{
			Failure: failure,
		}}
	} else if workflowContext.isWorkflowCompleted {
		// Workflow completion
		metricsScope.Counter(metrics.WorkflowCompletedCounter).Inc(1)
		closeCommand = createNewCommand(enumspb.COMMAND_TYPE_COMPLETE_WORKFLOW_EXECUTION)
		closeCommand.Attributes = &commandpb.Command_CompleteWorkflowExecutionCommandAttributes{CompleteWorkflowExecutionCommandAttributes: &commandpb.CompleteWorkflowExecutionCommandAttributes{
			Result: workflowContext.result,
		}}
	}

	if closeCommand != nil {
		commands = append(commands, closeCommand)
		elapsed := time.Since(workflowContext.workflowStartTime)
		metricsScope.Timer(metrics.WorkflowEndToEndLatency).Record(elapsed)
		forceNewWorkflowTask = false
	}

	var queryResults map[string]*querypb.WorkflowQueryResult
	if len(task.Queries) != 0 {
		queryResults = make(map[string]*querypb.WorkflowQueryResult)
		for queryID, query := range task.Queries {
			result, err := eventHandler.ProcessQuery(query.GetQueryType(), query.QueryArgs)
			if err != nil {
				queryResults[queryID] = &querypb.WorkflowQueryResult{
					ResultType:   enumspb.QUERY_RESULT_TYPE_FAILED,
					ErrorMessage: err.Error(),
				}
			} else {
				queryResults[queryID] = &querypb.WorkflowQueryResult{
					ResultType: enumspb.QUERY_RESULT_TYPE_ANSWERED,
					Answer:     result,
				}
			}
		}
	}

	return &workflowservice.RespondWorkflowTaskCompletedRequest{
		TaskToken:                  task.TaskToken,
		Commands:                   commands,
		Identity:                   wth.identity,
		ReturnNewWorkflowTask:      true,
		ForceCreateNewWorkflowTask: forceNewWorkflowTask,
		BinaryChecksum:             getBinaryChecksum(),
		QueryResults:               queryResults,
	}
}

func errorToFailWorkflowTask(taskToken []byte, err error, identity string, dataConverter converter.DataConverter) *workflowservice.RespondWorkflowTaskFailedRequest {
	return &workflowservice.RespondWorkflowTaskFailedRequest{
		TaskToken:      taskToken,
		Cause:          enumspb.WORKFLOW_TASK_FAILED_CAUSE_WORKFLOW_WORKER_UNHANDLED_FAILURE,
		Failure:        convertErrorToFailure(err, dataConverter),
		Identity:       identity,
		BinaryChecksum: getBinaryChecksum(),
	}
}

func (wth *workflowTaskHandlerImpl) executeAnyPressurePoints(event *historypb.HistoryEvent, isInReplay bool) error {
	if wth.ppMgr != nil && !reflect.ValueOf(wth.ppMgr).IsNil() && !isInReplay {
		switch event.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED:
			return wth.ppMgr.Execute(pressurePointTypeWorkflowTaskStartTimeout)
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			return wth.ppMgr.Execute(pressurePointTypeActivityTaskScheduleTimeout)
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED:
			return wth.ppMgr.Execute(pressurePointTypeActivityTaskStartTimeout)
		case enumspb.EVENT_TYPE_WORKFLOW_TASK_COMPLETED:
			return wth.ppMgr.Execute(pressurePointTypeWorkflowTaskCompleted)
		}
	}
	return nil
}

func newActivityTaskHandler(
	service workflowservice.WorkflowServiceClient,
	params workerExecutionParameters,
	registry *registry,
) ActivityTaskHandler {
	return newActivityTaskHandlerWithCustomProvider(service, params, registry, nil)
}

func newActivityTaskHandlerWithCustomProvider(
	service workflowservice.WorkflowServiceClient,
	params workerExecutionParameters,
	registry *registry,
	activityProvider activityProvider,
) ActivityTaskHandler {
	return &activityTaskHandlerImpl{
		taskQueueName:      params.TaskQueue,
		identity:           params.Identity,
		service:            service,
		logger:             params.Logger,
		metricsScope:       params.MetricsScope,
		userContext:        params.UserContext,
		registry:           registry,
		activityProvider:   activityProvider,
		dataConverter:      params.DataConverter,
		workerStopCh:       params.WorkerStopChannel,
		contextPropagators: params.ContextPropagators,
		tracer:             params.Tracer,
	}
}

type temporalInvoker struct {
	sync.Mutex
	identity            string
	service             workflowservice.WorkflowServiceClient
	metricsScope        tally.Scope
	taskToken           []byte
	cancelHandler       func()
	heartBeatTimeout    time.Duration // The heart beat interval configured for this activity.
	hbBatchEndTimer     *time.Timer   // Whether we started a batch of operations that need to be reported in the cycle. This gets started on a user call.
	lastDetailsToReport **commonpb.Payloads
	closeCh             chan struct{}
	workerStopChannel   <-chan struct{}
}

func (i *temporalInvoker) Heartbeat(details *commonpb.Payloads, skipBatching bool) error {
	i.Lock()
	defer i.Unlock()

	if i.hbBatchEndTimer != nil && !skipBatching {
		// If we have started batching window, keep track of last reported progress.
		i.lastDetailsToReport = &details
		return nil
	}

	isActivityCanceled, err := i.internalHeartBeat(details)

	// If the activity is canceled, the activity can ignore the cancellation and do its work
	// and complete. Our cancellation is co-operative, so we will try to heartbeat.
	if (err == nil || isActivityCanceled) && !skipBatching {
		// We have successfully sent heartbeat, start next batching window.
		i.lastDetailsToReport = nil

		// Create timer to fire before the threshold to report.
		deadlineToTrigger := i.heartBeatTimeout
		if deadlineToTrigger <= 0 {
			// If we don't have any heartbeat timeout configured.
			deadlineToTrigger = defaultHeartBeatInterval
		}

		// We set a deadline at 80% of the timeout.
		duration := time.Duration(0.8 * float64(deadlineToTrigger))
		i.hbBatchEndTimer = time.NewTimer(duration)

		go func() {
			select {
			case <-i.hbBatchEndTimer.C:
				// We are close to deadline.
			case <-i.workerStopChannel:
				// Activity worker is close to stop. This does the same steps as batch timer ends.
			case <-i.closeCh:
				// We got closed.
				return
			}

			// We close the batch and report the progress.
			var detailsToReport **commonpb.Payloads

			i.Lock()
			detailsToReport = i.lastDetailsToReport
			i.hbBatchEndTimer.Stop()
			i.hbBatchEndTimer = nil
			i.Unlock()

			if detailsToReport != nil {
				// TODO: there is a potential race condition here as the lock is released here and
				// locked again in the Hearbeat() method. This possible that a heartbeat call from
				// user activity grabs the lock first and calls internalHeartBeat before this
				// batching goroutine, which means some activity progress will be lost.
				_ = i.Heartbeat(*detailsToReport, false)
			}
		}()
	}

	return err
}

func (i *temporalInvoker) internalHeartBeat(details *commonpb.Payloads) (bool, error) {
	isActivityCanceled := false
	timeout := i.heartBeatTimeout
	if timeout <= 0 {
		timeout = defaultHeartBeatInterval
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := recordActivityHeartbeat(ctx, i.service, i.metricsScope, i.identity, i.taskToken, details)

	switch err.(type) {
	case *CanceledError:
		// We are asked to cancel. inform the activity about cancellation through context.
		i.cancelHandler()
		isActivityCanceled = true

	case *serviceerror.NotFound, *serviceerror.NamespaceNotActive:
		// We will pass these through as cancellation for now but something we can change
		// later when we have setter on cancel handler.
		i.cancelHandler()
		isActivityCanceled = true
	}

	// We don't want to bubble temporary errors to the user.
	// This error won't be return to user check RecordActivityHeartbeat().
	return isActivityCanceled, err
}

func (i *temporalInvoker) Close(flushBufferedHeartbeat bool) {
	i.Lock()
	defer i.Unlock()
	close(i.closeCh)
	if i.hbBatchEndTimer != nil {
		i.hbBatchEndTimer.Stop()
		if flushBufferedHeartbeat && i.lastDetailsToReport != nil {
			_, _ = i.internalHeartBeat(*i.lastDetailsToReport)
			i.lastDetailsToReport = nil
		}
	}
}

func (i *temporalInvoker) GetClient(options ClientOptions) Client {
	return NewServiceClient(i.service, nil, options)
}

func newServiceInvoker(
	taskToken []byte,
	identity string,
	service workflowservice.WorkflowServiceClient,
	metricsScope tally.Scope,
	cancelHandler func(),
	heartBeatTimeout time.Duration,
	workerStopChannel <-chan struct{},
) ServiceInvoker {
	return &temporalInvoker{
		taskToken:         taskToken,
		identity:          identity,
		service:           service,
		metricsScope:      metricsScope,
		cancelHandler:     cancelHandler,
		heartBeatTimeout:  heartBeatTimeout,
		closeCh:           make(chan struct{}),
		workerStopChannel: workerStopChannel,
	}
}

// Execute executes an implementation of the activity.
func (ath *activityTaskHandlerImpl) Execute(taskQueue string, t *workflowservice.PollActivityTaskQueueResponse) (result interface{}, err error) {
	traceLog(func() {
		ath.logger.Debug("Processing new activity task",
			tagWorkflowID, t.WorkflowExecution.GetWorkflowId(),
			tagRunID, t.WorkflowExecution.GetRunId(),
			tagActivityType, t.ActivityType.GetName())
	})

	rootCtx := ath.userContext
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	canCtx, cancel := context.WithCancel(rootCtx)
	defer cancel()

	invoker := newServiceInvoker(t.TaskToken, ath.identity, ath.service, ath.metricsScope, cancel, common.DurationValue(t.GetHeartbeatTimeout()), ath.workerStopCh)
	defer func() {
		_, activityCompleted := result.(*workflowservice.RespondActivityTaskCompletedRequest)
		invoker.Close(!activityCompleted) // flush buffered heartbeat if activity was not successfully completed.
	}()

	workflowType := t.WorkflowType.GetName()
	activityType := t.ActivityType.GetName()
	activityMetricsScope := metrics.GetMetricsScopeForActivity(ath.metricsScope, workflowType, activityType)
	ctx := WithActivityTask(canCtx, t, taskQueue, invoker, ath.logger, activityMetricsScope, ath.dataConverter, ath.workerStopCh, ath.contextPropagators, ath.tracer)

	activityImplementation := ath.getActivity(activityType)
	if activityImplementation == nil {
		// Couldn't find the activity implementation.
		supported := strings.Join(ath.getRegisteredActivityNames(), ", ")
		return nil, fmt.Errorf("unable to find activityType=%v. Supported types: [%v]", activityType, supported)
	}

	// panic handler
	defer func() {
		if p := recover(); p != nil {
			topLine := fmt.Sprintf("activity for %s [panic]:", ath.taskQueueName)
			st := getStackTraceRaw(topLine, 7, 0)
			ath.logger.Error("Activity panic.",
				tagWorkflowID, t.WorkflowExecution.GetWorkflowId(),
				tagRunID, t.WorkflowExecution.GetRunId(),
				tagActivityType, activityType,
				"PanicError", fmt.Sprintf("%v", p),
				"PanicStack", st)
			activityMetricsScope.Counter(metrics.ActivityTaskErrorCounter).Inc(1)
			panicErr := newPanicError(p, st)
			result, err = convertActivityResultToRespondRequest(ath.identity, t.TaskToken, nil, panicErr, ath.dataConverter), nil
		}
	}()

	// propagate context information into the activity context from the headers
	for _, ctxProp := range ath.contextPropagators {
		var err error
		if ctx, err = ctxProp.Extract(ctx, NewHeaderReader(t.Header)); err != nil {
			return nil, fmt.Errorf("unable to propagate context: %w", err)
		}
	}

	info := ctx.Value(activityEnvContextKey).(*activityEnvironment)
	ctx, dlCancelFunc := context.WithDeadline(ctx, info.deadline)
	defer dlCancelFunc()

	ctx, span := createOpenTracingActivitySpan(ctx, ath.tracer, time.Now(), activityType, t.WorkflowExecution.GetWorkflowId(), t.WorkflowExecution.GetRunId())
	defer span.Finish()
	output, err := activityImplementation.Execute(ctx, t.Input)

	dlCancelFunc()
	if <-ctx.Done(); ctx.Err() == context.DeadlineExceeded {
		ath.logger.Info("Activity complete after timeout.",
			tagWorkflowID, t.WorkflowExecution.GetWorkflowId(),
			tagRunID, t.WorkflowExecution.GetRunId(),
			tagActivityType, activityType,
			tagResult, output,
			tagError, err,
		)
		return nil, ctx.Err()
	}
	if err != nil && err != ErrActivityResultPending {
		ath.logger.Error("Activity error.",
			tagWorkflowID, t.WorkflowExecution.GetWorkflowId(),
			tagRunID, t.WorkflowExecution.GetRunId(),
			tagActivityType, activityType,
			tagError, err,
		)
	}
	return convertActivityResultToRespondRequest(ath.identity, t.TaskToken, output, err, ath.dataConverter), nil
}

func (ath *activityTaskHandlerImpl) getActivity(name string) activity {
	if ath.activityProvider != nil {
		return ath.activityProvider(name)
	}

	if a, ok := ath.registry.GetActivity(name); ok {
		return a
	}

	return nil
}

func (ath *activityTaskHandlerImpl) getRegisteredActivityNames() (activityNames []string) {
	for _, a := range ath.registry.getRegisteredActivities() {
		activityNames = append(activityNames, a.ActivityType().Name)
	}
	return
}

func createNewCommand(commandType enumspb.CommandType) *commandpb.Command {
	return &commandpb.Command{
		CommandType: commandType,
	}
}

func recordActivityHeartbeat(
	ctx context.Context,
	service workflowservice.WorkflowServiceClient,
	metricsScope tally.Scope,
	identity string,
	taskToken []byte,
	details *commonpb.Payloads,
) error {
	request := &workflowservice.RecordActivityTaskHeartbeatRequest{
		TaskToken: taskToken,
		Details:   details,
		Identity:  identity}

	var heartbeatResponse *workflowservice.RecordActivityTaskHeartbeatResponse
	heartbeatErr := backoff.Retry(ctx,
		func() error {
			grpcCtx, cancel := newGRPCContext(ctx)
			defer cancel()

			var err error
			heartbeatResponse, err = service.RecordActivityTaskHeartbeat(grpcCtx, request)
			return err
		}, createDynamicServiceRetryPolicy(ctx), isServiceTransientError)

	if heartbeatErr == nil && heartbeatResponse != nil && heartbeatResponse.GetCancelRequested() {
		return NewCanceledError()
	}

	return heartbeatErr
}

func recordActivityHeartbeatByID(
	ctx context.Context,
	service workflowservice.WorkflowServiceClient,
	metricsScope tally.Scope,
	identity string,
	namespace, workflowID, runID, activityID string,
	details *commonpb.Payloads,
) error {
	request := &workflowservice.RecordActivityTaskHeartbeatByIdRequest{
		Namespace:  namespace,
		WorkflowId: workflowID,
		RunId:      runID,
		ActivityId: activityID,
		Details:    details,
		Identity:   identity}

	var heartbeatResponse *workflowservice.RecordActivityTaskHeartbeatByIdResponse
	heartbeatErr := backoff.Retry(ctx,
		func() error {
			grpcCtx, cancel := newGRPCContext(ctx)
			defer cancel()

			var err error
			heartbeatResponse, err = service.RecordActivityTaskHeartbeatById(grpcCtx, request)
			return err
		}, createDynamicServiceRetryPolicy(ctx), isServiceTransientError)

	if heartbeatErr == nil && heartbeatResponse != nil && heartbeatResponse.GetCancelRequested() {
		return NewCanceledError()
	}

	return heartbeatErr
}

// This enables verbose logging in the client library.
// check worker.EnableVerboseLogging()
func traceLog(fn func()) {
	if enableVerboseLogging {
		fn()
	}
}

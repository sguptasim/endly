package endly

import (
	"errors"
	"fmt"
	"github.com/viant/neatly"
	"github.com/viant/toolbox"
	"github.com/viant/toolbox/data"
	"github.com/viant/toolbox/url"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	//WorkflowServiceID represent workflow service id
	WorkflowServiceID = "workflow"
	//WorkflowEvalRunCriteriaEventType event Id
	WorkflowEvalRunCriteriaEventType = "EvalRunCriteria"
)

//WorkflowServiceActivity represents workflow activity
type WorkflowServiceActivity struct {
	Workflow string
	Service  string
	Action   string
	Tag      string
	TagIndex string
	TagId    string

	Description     string
	TagDescription  string
	Error           string
	StartTime       time.Time
	Ineligible      bool
	ServiceRequest  interface{}
	ServiceResponse interface{}
}

func TagId(workflow, tag, index, subpath string) string {
	return fmt.Sprintf("%v%v%v%v", workflow, tag, index, subpath)
}

//FormatTag return a formatted tag
func (a *WorkflowServiceActivity) FormatTag() string {
	if a.TagIndex != "" {
		return "[" + a.Tag + a.TagIndex + "]"
	}
	return "[" + a.Tag + "]"
}

//WorkflowServiceActivityEndEventType represents activity end event type.
type WorkflowServiceActivityEndEventType struct {
}

type workflowService struct {
	*AbstractService
	Dao      *WorkflowDao
	registry map[string]*Workflow
}

func (s *workflowService) Register(workflow *Workflow) error {
	err := workflow.Validate()
	if err != nil {
		return err
	}
	s.registry[workflow.Name] = workflow
	return nil
}

func (s *workflowService) HasWorkflow(name string) bool {
	_, found := s.registry[name]
	return found
}

func (s *workflowService) Workflow(name string) (*Workflow, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if result, found := s.registry[name]; found {
		return result, nil
	}
	return nil, fmt.Errorf("Failed to lookup workflow: %v", name)
}

func (s *workflowService) evaluateRunCriteria(context *Context, criteria string) (bool, error) {
	if criteria == "" {
		return true, nil
	}

	colonPosition := strings.Index(criteria, ":")
	if colonPosition == -1 {
		return false, fmt.Errorf("Run criteria needs to have colon: but had: %v", criteria)
	}
	fragments := strings.Split(criteria, ":")
	actualOperand := context.Expand(strings.TrimSpace(fragments[0]))
	expectedOperand := context.Expand(strings.TrimSpace(fragments[1]))
	validator := &Validator{}
	var result, err = validator.Check(expectedOperand, actualOperand)
	s.AddEvent(context, WorkflowEvalRunCriteriaEventType, Pairs("actual", actualOperand, "expected", expectedOperand, "eligible", result), Debug)
	return result, err
}

func isTaskAllowed(candidate *WorkflowTask, request *WorkflowRunRequest) (bool, map[int]bool) {
	if request.Tasks == "" || request.Tasks == "*" {
		return true, nil
	}
	var actions map[int]bool
	var encodedTask []string
	tasks := strings.Split(request.Tasks, ",")
	for _, task := range tasks {
		encodedTask = nil
		var taskName = task
		if !strings.Contains(task, "=") {
			encodedTask = strings.Split(task, "=")
			taskName = encodedTask[0]
		}
		if taskName == candidate.Name {
			if len(encodedTask) == 2 {
				actions = make(map[int]bool)
				for _, allowedIndex := range strings.Split(encodedTask[1], ":") {
					actions[toolbox.AsInt(allowedIndex)] = true
				}
			}
			return true, actions
		}
	}
	return false, nil
}

func (s *workflowService) addVariableEvent(name string, variables Variables, context *Context, state data.Map) {
	if len(variables) == 0 {
		return
	}
	var values = make(map[string]interface{})
	for _, variable := range variables {
		var name = variable.Name
		name = strings.Replace(name, "->", "", 1)
		values[name], _ = state.GetValue(name)
	}
	s.AddEvent(context, name, Pairs("variables", variables, "values", values), Debug)
}

func (s *workflowService) loadWorkflowIfNeeded(context *Context, name string, URL string) (err error) {
	if !s.HasWorkflow(name) {
		var workflowResource *url.Resource
		if URL == "" {
			workflowResource, err = s.Dao.NewRepoResource(context.state, fmt.Sprintf("workflow/%v.csv", name))
			if err != nil {
				return err
			}
		} else {
			workflowResource = url.NewResource(URL)
		}

		_, err := s.loadWorkflow(context, &WorkflowLoadRequest{Source: workflowResource})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *workflowService) asServiceRequest(action *ServiceAction, serviceRequest interface{}, requestMap map[string]interface{}) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("Failed to cast %v into %T, due to %v", requestMap, serviceRequest, r)
		}
	}()

	err = converter.AssignConverted(serviceRequest, requestMap)
	if err != nil {
		return fmt.Errorf("Failed to create request %v on %v.%v, %v", requestMap, action.Service, action.Action, err)
	}
	return err

}

func (s *workflowService) runAction(context *Context, action *ServiceAction) error {
	var state = context.state

	serviceActivity := &WorkflowServiceActivity{
		Workflow:       context.Workflows.Last().Name,
		Action:         action.Action,
		Service:        action.Service,
		TagIndex:       action.TagIndex,
		TagId:          action.TagId,
		Description:    context.Expand(action.Description),
		TagDescription: context.Expand(action.TagDescription),
		Tag:            action.Tag,
		ServiceRequest: action.Request,
		StartTime:      time.Now(),
	}
	state.Put("activity", serviceActivity)
	var responseMap = make(map[string]interface{})
	serviceActivity.ServiceResponse = responseMap
	startEvent := s.Begin(context, action, Pairs("activity", serviceActivity), Info)
	defer s.End(context)(startEvent, Pairs("value", &WorkflowServiceActivityEndEventType{}, "response", responseMap))
	canRun, err := s.evaluateRunCriteria(context, action.RunCriteria)
	if err != nil {
		return err
	}
	if !canRun {
		serviceActivity.Ineligible = true
		return nil
	}

	err = action.Init.Apply(state, state)
	s.addVariableEvent("Action.Init", action.Init, context, state)
	if err != nil {
		return err
	}
	service, err := context.Service(action.Service)

	if err != nil {
		return err
	}

	expandedRequest := state.Expand(action.Request)
	if expandedRequest == nil || !toolbox.IsMap(expandedRequest) {
		return fmt.Errorf("Failed to evaluate request: %v, expected map but had: %T", expandedRequest, expandedRequest)
	}
	requestMap := toolbox.AsMap(expandedRequest)
	serviceRequest, err := service.NewRequest(action.Action)
	if err != nil {
		return err
	}

	serviceActivity.ServiceRequest = serviceRequest

	err = s.asServiceRequest(action, serviceRequest, requestMap)
	if err != nil {
		return err
	}
	serviceResponse := service.Run(context, serviceRequest)
	serviceActivity.ServiceResponse = serviceResponse

	if serviceResponse.Error != "" {
		var err = reportError(errors.New(serviceResponse.Error))
		return err
	}

	if serviceResponse.Response != nil {
		converter.AssignConverted(responseMap, serviceResponse.Response)
	}
	err = action.Post.Apply(data.Map(responseMap), state) //result to task  state
	s.addVariableEvent("Action.Post", action.Post, context, state)
	if err != nil {
		return err
	}
	s.Sleep(context, int(action.SleepInMs))
	return nil
}

func (s *workflowService) runTask(context *Context, workflow *Workflow, task *WorkflowTask, request *WorkflowRunRequest) error {
	var startTime = time.Now()
	var state = context.state
	state.Put(":task", task)
	var taskAllowed, allowedServiceActions = isTaskAllowed(task, request)
	if !taskAllowed {
		return nil
	}
	var hasAllowedActions = len(allowedServiceActions) > 0
	err := task.Init.Apply(state, state)
	s.addVariableEvent("Task.Init", task.Init, context, state)
	if err != nil {
		return err
	}

	canRun, err := s.evaluateRunCriteria(context, task.RunCriteria)
	if err != nil {
		return err
	}
	if !canRun {
		return nil
	}
	startEvent := s.Begin(context, task, Pairs("Id", task.Name))
	defer s.End(context)(startEvent, Pairs())

	var asyncActions = make([]*ServiceAction, 0)

	for i, action := range task.Actions {
		if action.Async {
			asyncActions = append(asyncActions, action)
			var asyncEvent = &AsyncServiceActionEvent{
				Workflow:    workflow.Name,
				Task:        context.Expand(task.Name),
				Description: context.Expand(action.Description),
				Service:     action.Service,
				Action:      action.Action,
				TagId:       action.TagId,
			}

			s.AddEvent(context, asyncEvent, Pairs("value", asyncEvent))
			continue
		}
		if hasAllowedActions && !allowedServiceActions[i] {
			continue
		}
		err = s.runAction(context, action)
		if err != nil {
			return fmt.Errorf("Failed to run action:%v %v", action.Tag, err)
		}
	}

	err = s.runAsyncActions(context, workflow, task, request, asyncActions)
	if err != nil {
		return err
	}
	err = task.Post.Apply(state, state)
	s.addVariableEvent("Task.Post", task.Post, context, state)
	if task.TimeSpentMs > 0 {
		var elapsed = (time.Now().UnixNano() - startTime.UnixNano()) / int64(time.Millisecond)
		var remainingExecutionTime = time.Duration(task.TimeSpentMs - int(elapsed))
		s.Sleep(context, int(remainingExecutionTime))
	}
	return err
}

func (s *workflowService) runAsyncActions(context *Context, workflow *Workflow, task *WorkflowTask, request *WorkflowRunRequest, asyncAction []*ServiceAction) error {
	var err error
	if len(asyncAction) > 0 {
		group := sync.WaitGroup{}
		group.Add(len(asyncAction))
		var groupErr error
		s.Sleep(context, 200)
		for _, action := range asyncAction {
			go func(actionContext *Context, action *ServiceAction) {
				defer group.Done()
				defer s.publishEvents(context, actionContext.Events.Events)
				defer actionContext.Clone()
				actionContext.MakeAsyncSafe()
				err = s.runAction(actionContext, action)
				if err != nil {
					groupErr = fmt.Errorf("Failed to run action:%v %v", action.Tag, err)
				}

			}(context.Clone(), action)
		}

		group.Wait()

		if groupErr != nil {
			return groupErr
		}
	}
	return err
}
func (s *workflowService) publishEvents(context *Context, events []*Event) {
	if len(events) > 0 {
		s.mutex.Lock()
		defer s.mutex.Unlock()
		for _, event := range events {
			context.Events.Push(event)
			if context.EventLogger != nil {
				context.EventLogger.Log(event)
			}
		}
	}
}

func (s *workflowService) runWorkflow(upstreamContext *Context, request *WorkflowRunRequest) (*WorkflowRunResponse, error) {
	if request.EnableLogging {
		upstreamContext.EventLogger = NewEventLogger(path.Join(request.LoggingDirectory, upstreamContext.SessionID))
	}

	var err = s.loadWorkflowIfNeeded(upstreamContext, request.Name, request.WorkflowURL)
	if err != nil {
		return nil, err
	}

	workflow, err := s.Workflow(request.Name)
	if err != nil {
		return nil, err
	}
	s.AddEvent(upstreamContext, "Workflow.Loaded", Pairs("workflow", workflow))
	upstreamContext.Workflows.Push(workflow)
	defer upstreamContext.Workflows.Pop()

	var response = &WorkflowRunResponse{
		SessionID: upstreamContext.SessionID,
		Data:      make(map[string]interface{}),
	}

	parentWorkflow := upstreamContext.Workflow()
	if parentWorkflow != nil {
		upstreamContext.Put(workflowKey, parentWorkflow)
	} else {
		upstreamContext.Put(workflowKey, workflow)
	}

	context := upstreamContext.Clone()
	var state = context.State()

	if workflow.Source.URL == "" {
		return nil, fmt.Errorf("workflow.Source was empty %v", workflow.Name)
	}

	var workflowData = data.Map(workflow.Data)
	state.Put(neatly.OwnerURL, workflow.Source.URL)
	state.Put("data", workflowData)

	params := buildParamsMap(request, context)

	if request.PublishParameters {
		for key, value := range params {
			state.Put(key, state.Expand(value))
		}
	}
	state.Put("params", params)
	err = workflow.Init.Apply(state, state)
	s.addVariableEvent("Workflow.Init", workflow.Init, context, state)
	if err != nil {
		return nil, err
	}
	s.AddEvent(context, "State.Init", Pairs("state", state.AsEncodableMap()), Debug)
	for _, task := range workflow.Tasks {

		err = s.runTask(context, workflow, task, request)
		if err != nil {
			return nil, err
		}

	}
	workflow.Post.Apply(state, response.Data) //context -> workflow output
	s.addVariableEvent("Workflow.Post", workflow.Post, context, state)

	if workflow.SleepInMs > 0 {
		s.Sleep(context, workflow.SleepInMs)
	}
	return response, nil
}

func buildParamsMap(request *WorkflowRunRequest, context *Context) data.Map {
	var params = data.NewMap()
	if len(request.Params) > 0 {
		for k, v := range request.Params {
			if toolbox.IsString(v) {
				params[k] = context.Expand(toolbox.AsString(v))
			} else {
				params[k] = v
			}
		}
	}
	return params
}

func (s *workflowService) loadWorkflow(context *Context, request *WorkflowLoadRequest) (*WorkflowLoadResponse, error) {
	workflow, err := s.Dao.Load(context, request.Source)
	if err != nil {
		return nil, fmt.Errorf("Failed to load workflow: %v, %v", request.Source, err)
	}
	s.Mutex().Lock()
	defer  s.Mutex().Unlock()
	err = s.Register(workflow)
	if err != nil {
		return nil, fmt.Errorf("Failed to register workflow: %v, %v", request.Source, err)
	}
	return &WorkflowLoadResponse{
		Workflow: workflow,
	}, nil
}

func (s *workflowService) removeSession(context *Context) {
	go func() {
		time.Sleep(2 * time.Second)
		s.Mutex().Lock()
		defer s.Mutex().Unlock()
		s.state.Delete(context.SessionID)
	}()
}

func (s *workflowService) startSession(context *Context) bool {
	s.Mutex().RLock()
	if s.state.Has(context.SessionID) {
		s.Mutex().RUnlock()
		return false
	}
	s.Mutex().RUnlock()
	s.state.Put(context.SessionID, context)
	s.Mutex().Lock()
	defer s.Mutex().Unlock()
	return true
}

func (s *workflowService) isAsyncRequest(request interface{}) bool {
	if runRequest, ok := request.(*WorkflowRunRequest); ok {
		return runRequest.Async
	}
	return false
}

func (s *workflowService) reportErrorIfNeeded(context *Context, response *ServiceResponse) {
	if response.Error != "" {
		var errorEventType = &ErrorEventType{Error: response.Error}
		s.AddEvent(context, errorEventType, Pairs("value", errorEventType), Info)
	}
}

func (s *workflowService) Run(context *Context, request interface{}) *ServiceResponse {
	startedSession := s.startSession(context)
	startEvent := s.Begin(context, request, Pairs("request", request))
	var response = &ServiceResponse{Status: "ok"}
	defer s.reportErrorIfNeeded(context, response)

	if !s.isAsyncRequest(request) {
		defer s.End(context)(startEvent, Pairs("response", response))
	}
	var err error
	switch actualRequest := request.(type) {
	case *WorkflowRunRequest:
		if actualRequest.Async {
			go func() {
				if startedSession {
					defer s.reportErrorIfNeeded(context, response)
					defer s.removeSession(context)
				}
				_, err := s.runWorkflow(context, actualRequest)
				if err != nil {
					var eventType = &ErrorEventType{Error: fmt.Sprintf("%v", err)}
					s.AddEvent(context, eventType, Pairs("value", eventType), Info)
				}
				s.End(context)(startEvent, Pairs("response", response))
			}()

			response.Response = &WorkflowRunResponse{
				SessionID: context.SessionID,
			}
			return response
		}
		response.Response, err = s.runWorkflow(context, actualRequest)
		if err != nil {
			response.Error = fmt.Sprintf("Failed to run workflow: %v, %v", actualRequest.Name, err)
		}
	case *WorkflowRegisterRequest:
		err := s.Register(actualRequest.Workflow)
		if err != nil {
			response.Error = fmt.Sprintf("Failed to register workflow: %v, %v", actualRequest.Workflow.Name, err)
		}
	case *WorkflowLoadRequest:
		response.Response, err = s.loadWorkflow(context, actualRequest)
		if err != nil {
			response.Error = fmt.Sprintf("%v", err)
		}
	default:
		response.Error = fmt.Sprintf("Unsupported request type: %T", request)
	}
	if response.Error != "" {
		response.Status = "err"
	}
	return response
}

func (s *workflowService) NewRequest(action string) (interface{}, error) {
	switch action {
	case "run":
		return &WorkflowRunRequest{}, nil
	case "register":
		return &WorkflowRegisterRequest{}, nil
	case "load":
		return &WorkflowLoadRequest{}, nil
	}
	return s.AbstractService.NewRequest(action)
}

//NewWorkflowService returns a new workflow service.
func NewWorkflowService() Service {
	var result = &workflowService{
		AbstractService: NewAbstractService(WorkflowServiceID),
		Dao:             NewWorkflowDao(),
		registry:        make(map[string]*Workflow),
	}
	result.AbstractService.Service = result
	return result
}

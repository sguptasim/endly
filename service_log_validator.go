package endly

import (
	"bytes"
	"fmt"
	"github.com/viant/assertly"
	"github.com/viant/toolbox"
	"github.com/viant/toolbox/data"
	"github.com/viant/toolbox/storage"
	"github.com/viant/toolbox/url"
	"io"
	"io/ioutil"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	//LogValidatorServiceID represents log validator service id.
	LogValidatorServiceID = "validator/log"
)

type logValidatorService struct {
	*AbstractService
}

//LogRecordAssert represents log record assert
type LogRecordAssert struct {
	TagID    string
	Expected interface{}
	Actual   interface{}
}


//LogProcessingState represents log processing state
type LogProcessingState struct {
	Line     int
	Position int
}

//Update updates processed position and line number
func (s *LogProcessingState) Update(position, lineNumber int) (string, int) {
	s.Line = lineNumber
	s.Position += position
	return "", 0
}

//Reset resets processing state
func (s *LogProcessingState) Reset() {
	s.Line = 0
	s.Position = 0
}

//LogRecord repesents a log record
type LogRecord struct {
	URL    string
	Number int
	Line   string
}

//IndexedLogRecord represents indexed log record
type IndexedLogRecord struct {
	*LogRecord
	IndexValue string
}

//AsMap returns log records as map
func (r *LogRecord) AsMap() (map[string]interface{}, error) {
	var result = make(map[string]interface{})
	err := toolbox.NewJSONDecoderFactory().Create(strings.NewReader(r.Line)).Decode(&result)
	return result, err
}

//LogFile represents a log file
type LogFile struct {
	URL             string
	Content         string
	Name            string
	*LogType
	ProcessingState *LogProcessingState
	LastModified    time.Time
	Size            int
	Records         []*LogRecord
	IndexedRecords  map[string]*LogRecord
	Mutex           *sync.RWMutex
}

//ShiftLogRecord returns and remove the first log record if present
func (f *LogFile) ShiftLogRecord() *LogRecord {
	f.Mutex.Lock()
	defer f.Mutex.Unlock()
	if len(f.Records) == 0 {
		return nil
	}
	result := f.Records[0]
	f.Records = f.Records[1:]
	return result
}

//ShiftLogRecordByIndex returns and remove the first log record if present
func (f *LogFile) ShiftLogRecordByIndex(value string) *LogRecord {
	f.Mutex.Lock()
	defer f.Mutex.Unlock()
	if len(f.Records) == 0 {
		return nil
	}
	result, has := f.IndexedRecords[value]
	if !has {
		result = f.Records[0]
		f.Records = f.Records[1:]
	} else {
		var records = make([]*LogRecord, 0)
		for _, candidate := range f.Records {
			if candidate == result {
				continue
			}
			records = append(records, candidate)
		}
		f.Records = records
	}
	return result
}

//PushLogRecord appends provided log record to the records.
func (f *LogFile) PushLogRecord(record *LogRecord) {
	f.Mutex.Lock()
	defer f.Mutex.Unlock()

	if len(f.Records) == 0 {
		f.Records = make([]*LogRecord, 0)
	}
	f.Records = append(f.Records, record)
	if f.UseIndex() {
		if expr, err := f.GetIndexExpr(); err == nil {
			var indexValue = matchLogIndex(expr, record.Line)
			if indexValue != "" {
				f.IndexedRecords[indexValue] = record
			}
		}
	}
}

func matchLogIndex(expr *regexp.Regexp, input string) string {
	if expr.MatchString(input) {
		matches := expr.FindStringSubmatch(input)
		if len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

//Reset resets processing state
func (f *LogFile) Reset(object storage.Object) {
	f.Mutex.Lock()
	defer f.Mutex.Unlock()
	f.Size = int(object.FileInfo().Size())
	f.LastModified = object.FileInfo().ModTime()
	f.ProcessingState.Reset()
}

//HasPendingLogs returns true if file has pending validation records
func (f *LogFile) HasPendingLogs() bool {
	f.Mutex.Lock()
	defer f.Mutex.Unlock()
	return len(f.Records) > 0
}

func (f *LogFile) readLogRecords(reader io.Reader) error {
	data, err := ioutil.ReadAll(reader)
	if err != nil {
		return err
	}
	if f.ProcessingState.Position > len(data) {
		return nil
	}
	var line = ""
	var startPosition = f.ProcessingState.Position
	var startLine = f.ProcessingState.Line
	var lineIndex = startLine
	var dataProcessed = 0
	for i := startPosition; i < len(data); i++ {
		dataProcessed++
		aChar := string(data[i])
		if aChar != "\n" && aChar != "\r" {
			line += aChar
			continue
		}

		line = strings.Trim(line, " \r\t")
		lineIndex++
		if f.Exclusion != "" {
			if strings.Contains(line, f.Exclusion) {
				line, dataProcessed = f.ProcessingState.Update(dataProcessed, lineIndex)
				continue
			}
		}
		if f.Inclusion != "" {
			if !strings.Contains(line, f.Inclusion) {
				line, dataProcessed = f.ProcessingState.Update(dataProcessed, lineIndex)
				continue
			}
		}

		if len(line) > 0 {
			f.PushLogRecord(&LogRecord{
				URL:    f.URL,
				Line:   line,
				Number: lineIndex,
			})
		}
		if err != nil {
			return err
		}
		line, dataProcessed = f.ProcessingState.Update(dataProcessed, lineIndex)
	}
	return nil
}

//LogTypeMeta represents a log type meta
type LogTypeMeta struct {
	Source   *url.Resource
	LogType  *LogType
	LogFiles map[string]*LogFile
}

type logRecordIterator struct {
	logFileProvider func() []*LogFile
	logFiles        []*LogFile
	logFileIndex    int
}

//HasNext returns true if iterator has next element.
func (i *logRecordIterator) HasNext() bool {
	var logFileCount = len(i.logFiles)
	if i.logFileIndex >= logFileCount {
		i.logFiles = i.logFileProvider()
		for j, candidate := range i.logFiles {
			if candidate.HasPendingLogs() {
				i.logFileIndex = j
				return true
			}
		}
		return false
	}

	logFile := i.logFiles[i.logFileIndex]
	if !logFile.HasPendingLogs() {
		i.logFileIndex++
		return i.HasNext()
	}
	return true
}

func (s *logValidatorService) reset(context *Context, request *LogValidatorResetRequest) (*LogValidatorResetResponse, error) {
	var response = &LogValidatorResetResponse{
		LogFiles: make([]string, 0),
	}
	for _, logTypeName := range request.LogTypes {
		if !s.state.Has(logTypeMetaKey(logTypeName)) {
			continue
		}
		if logTypeMeta, ok := s.state.Get(logTypeMetaKey(logTypeName)).(*LogTypeMeta); ok {
			for _, logFile := range logTypeMeta.LogFiles {
				logFile.ProcessingState = &LogProcessingState{
					Position: logFile.Size,
					Line:     len(logFile.Records),
				}
				logFile.Records = make([]*LogRecord, 0)
				response.LogFiles = append(response.LogFiles, logFile.Name)
			}
		}
	}
	return response, nil
}

func (s *logValidatorService) assert(context *Context, request *LogValidatorAssertRequest) (*LogValidatorAssertResponse, error) {
	var response = &LogValidatorAssertResponse{
		Description: request.Description,
		Validations: make([]*assertly.Validation, 0),
	}
	var state = s.State()
	if len(request.ExpectedLogRecords) == 0 {
		return response, nil
	}

	if request.LogWaitTimeMs == 0 {
		request.LogWaitTimeMs = 500
	}
	if request.LogWaitRetryCount == 0 {
		request.LogWaitRetryCount = 3
	}

	for _, expectedLogRecords := range request.ExpectedLogRecords {
		logTypeMeta, err := s.getLogTypeMeta(expectedLogRecords, state)
		if err != nil {
			return nil, err
		}

		var logRecordIterator = logTypeMeta.LogRecordIterator()
		logWaitRetryCount := request.LogWaitRetryCount
		logWaitDuration := time.Duration(request.LogWaitTimeMs) * time.Millisecond

		for _, expectedLogRecord := range expectedLogRecords.Records {
			var validation = &assertly.Validation{
				TagID:       expectedLogRecords.TagID,
				Description: fmt.Sprintf("Log Validation: %v", expectedLogRecords.Type),
			}
			response.Validations = append(response.Validations, validation)
			for j := 0; j < logWaitRetryCount; j++ {
				if logRecordIterator.HasNext() {
					break
				}
				var sleepEventType = &SleepEventType{SleepTimeMs: int(logWaitDuration) / int(time.Millisecond)}
				AddEvent(context, sleepEventType, Pairs("value", sleepEventType))
				time.Sleep(logWaitDuration)
			}

			if !logRecordIterator.HasNext() {
				validation.AddFailure(assertly.NewFailure("", fmt.Sprintf("[%v]", expectedLogRecords.TagID), "missing log record", expectedLogRecord, nil))
				return response, nil
			}

			var logRecord = &LogRecord{}
			var isLogStructured = toolbox.IsMap(expectedLogRecord)
			var calledNext = false
			if logTypeMeta.LogType.UseIndex() {
				if expr, err := logTypeMeta.LogType.GetIndexExpr(); err == nil {
					var expectedTextRecord = toolbox.AsString(expectedLogRecord)
					if toolbox.IsMap(expectedLogRecord) || toolbox.IsSlice(expectedLogRecord) || toolbox.IsStruct(expectedLogRecord) {
						expectedTextRecord, _ = toolbox.AsJSONText(expectedLogRecord)
					}
					var indexValue = matchLogIndex(expr, expectedTextRecord)
					if indexValue != "" {
						indexedLogRecord := &IndexedLogRecord{
							IndexValue: indexValue,
						}
						err = logRecordIterator.Next(indexedLogRecord)
						if err != nil {
							return nil, err
						}
						calledNext = true
						logRecord = indexedLogRecord.LogRecord
					}
				}
			}

			if !calledNext {
				err = logRecordIterator.Next(&logRecord)
				if err != nil {
					return nil, err
				}
			}

			var actualLogRecord interface{} = logRecord.Line
			if isLogStructured {
				actualLogRecord, err = logRecord.AsMap()
				if err != nil {
					return nil, err
				}
			}
			logRecordsAssert := &LogRecordAssert{
				TagID:    expectedLogRecords.TagID,
				Expected: expectedLogRecord,
				Actual:   actualLogRecord,
			}
			_, filename := toolbox.URLSplit(logRecord.URL)
			logValidation, err := Assert(context, fmt.Sprintf("%v:%v", filename, logRecord.Number), expectedLogRecord, actualLogRecord)
			if err != nil {
				return nil, err
			}
			AddEvent(context, logRecordsAssert, Pairs("value", logRecordsAssert, "logValidation", logValidation))
			validation.MergeFrom(logValidation)
		}

	}
	return response, nil
}

func (s *logValidatorService) getLogTypeMeta(expectedLogRecords *ExpectedLogRecord, state data.Map) (*LogTypeMeta, error) {
	var key = logTypeMetaKey(expectedLogRecords.Type)
	s.Mutex().Lock()
	defer s.Mutex().Unlock()
	if !state.Has(key) {
		return nil, fmt.Errorf("failed to assert, unknown type:%v, please call listen function with requested log type", expectedLogRecords.Type)
	}
	logTypeMeta := state.Get(key).(*LogTypeMeta)
	return logTypeMeta, nil
}

func (s *logValidatorService) readLogFile(context *Context, source *url.Resource, service storage.Service, candidate storage.Object, logType *LogType) (*LogTypeMeta, error) {
	var result *LogTypeMeta
	var key = logTypeMetaKey(logType.Name)
	s.Mutex().Lock()

	if !s.state.Has(logTypeMetaKey(logType.Name)) {
		s.state.Put(key, NewLogTypeMeta(source, logType))
	}
	if logTypeMeta, ok := s.state.Get(key).(*LogTypeMeta); ok {
		result = logTypeMeta
	}

	var isNewLogFile = false

	_, name := toolbox.URLSplit(candidate.URL())
	logFile, has := result.LogFiles[name]
	fileInfo := candidate.FileInfo()
	if !has {
		isNewLogFile = true

		logFile = &LogFile{
			LogType:         logType,
			Name:            name,
			URL:             candidate.URL(),
			LastModified:    fileInfo.ModTime(),
			Size:            int(fileInfo.Size()),
			ProcessingState: &LogProcessingState{},
			Mutex:           &sync.RWMutex{},
			Records:         make([]*LogRecord, 0),
			IndexedRecords:  make(map[string]*LogRecord),
		}
		result.LogFiles[name] = logFile
	}
	s.Mutex().Unlock()
	if !isNewLogFile && (logFile.Size == int(fileInfo.Size()) && logFile.LastModified.Unix() == fileInfo.ModTime().Unix()) {
		return result, nil
	}
	reader, err := service.Download(candidate)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	logContent, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	var content = string(logContent)
	var fileOverridden = false
	if len(logFile.Content) > len(content) { //log shrink or rolled over case
		logFile.Reset(candidate)
		logFile.Content = content
		fileOverridden = true
	}

	if !fileOverridden && logFile.Size < int(fileInfo.Size()) && !strings.HasPrefix(content, string(logFile.Content)) {
		logFile.Reset(candidate)
	}

	logFile.Content = content
	logFile.Size = len(logContent)
	if len(logContent) > 0 {
		err = logFile.readLogRecords(bytes.NewReader(logContent))
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *logValidatorService) readLogFiles(context *Context, service storage.Service, source *url.Resource, logTypes ...*LogType) (LogTypesMeta, error) {
	var err error
	source, err = context.ExpandResource(source)
	if err != nil {
		return nil, err
	}

	var response LogTypesMeta = make(map[string]*LogTypeMeta)
	candidates, err := service.List(source.URL)
	if err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		if candidate.IsFolder() {
			continue
		}

		for _, logType := range logTypes {
			mask := strings.Replace(logType.Mask, "*", ".+", len(logType.Mask))
			maskExpression, err := regexp.Compile("^" + mask + "$")
			if err != nil {
				return nil, err
			}
			_, name := toolbox.URLSplit(candidate.URL())
			if maskExpression.MatchString(name) {
				logTypeMeta, err := s.readLogFile(context, source, service, candidate, logType)
				if err != nil {
					return nil, err
				}
				response[logType.Name] = logTypeMeta
			}
		}
	}
	return response, nil
}

func (s *logValidatorService) getStorageService(context *Context, resource *url.Resource) (storage.Service, error) {
	var state = context.state
	if state.Has(UseMemoryService) {
		return storage.NewMemoryService(), nil
	}
	return storage.NewServiceForURL(resource.URL, resource.Credential)
}

func (s *logValidatorService) listenForChanges(context *Context, request *LogValidatorListenRequest) error {
	var target, err = context.ExpandResource(request.Source)
	if err != nil {
		return err
	}
	service, err := s.getStorageService(context, target)
	if err != nil {
		return err
	}
	go func() {
		defer service.Close()
		frequency := time.Duration(request.FrequencyMs) * time.Millisecond
		if request.FrequencyMs <= 0 {
			frequency = 400 * time.Millisecond
		}
		for !context.IsClosed() {
			_, err := s.readLogFiles(context, service, request.Source, request.Types...)
			if err != nil {
				log.Printf("failed to load log types %v", err)
				break
			}
			time.Sleep(frequency)
		}

	}()
	return nil
}

func (s *logValidatorService) listen(context *Context, request *LogValidatorListenRequest) (*LogValidatorListenResponse, error) {
	var source, err = context.ExpandResource(request.Source)
	if err != nil {
		return nil, err
	}

	for _, logType := range request.Types {
		if s.state.Has(logTypeMetaKey(logType.Name)) {
			return nil, fmt.Errorf("listener has been already register for %v", logType.Name)
		}
	}
	service, err := storage.NewServiceForURL(request.Source.URL, request.Source.Credential)
	if err != nil {
		return nil, err
	}
	defer service.Close()
	logTypeMetas, err := s.readLogFiles(context, service, request.Source, request.Types...)
	if err != nil {
		return nil, err
	}
	for _, logType := range request.Types {
		logMeta, ok := logTypeMetas[logType.Name]
		if !ok {
			logMeta = NewLogTypeMeta(source, logType)
			logTypeMetas[logType.Name] = logMeta
		}
		s.state.Put(logTypeMetaKey(logType.Name), logMeta)
	}

	response := &LogValidatorListenResponse{
		Meta: logTypeMetas,
	}

	err = s.listenForChanges(context, request)
	return response, err
}

const (
	logValidatorExample = `{
  "FrequencyMs": 500,
  "Source": {
    "URL": "scp://127.0.0.1/opt/elogger/logs/",
    "Credential": "${env.HOME}/.secret/localhost.json"
  },
  "Types": [
    {
      "Name": "event1",
      "Format": "json",
      "Mask": "elog*.log",
      "Inclusion": "/event1/",
      "IndexRegExpr": "\"EventID\":\"([^\"]+)\""
    }
  ]
}`

	logValidatorAssertExample = ` {
		"LogWaitTimeMs": 5000,
		"LogWaitRetryCount": 5,
		"Description": "E-logger event log validation",
		"ExpectedLogRecords": [
			{
				"Type": "event1",
				"Records": [
					{
						"EventID": "84423348-1384-11e8-b0b4-ba004c285304",
						"EventType": "event1",
						"Request": {
							"Method": "GET",
							"URL": "http://127.0.0.1:8777/event1/?k10=v1\u0026k2=v2"
						}
					},
					{
						"EventID": "8441c4bc-1384-11e8-b0b4-ba004c285304",
						"EventType": "event1",
						"Request": {
							"Method": "GET",
							"URL": "http://127.0.0.1:8777/event1/?k1=v1\u0026k2=v2"
						}
					}
				]
			},
			{
				"Type": "event2",
				"Records": [
					{
						"EventID": "84426d4a-1384-11e8-b0b4-ba004c285304",
						"EventType": "event2",
						"Request": {
							"Method": "GET",
							"URL": "http://127.0.0.1:8777/event2/?k1=v1\u0026k2=v2"
						}
					}
				]
			}
		]
	}`
)

func (s *logValidatorService) registerRoutes() {
	s.Register(&ServiceActionRoute{
		Action: "listen",
		RequestInfo: &ActionInfo{
			Description: "check for log changes",
			Examples: []*ExampleUseCase{
				{
					UseCase: "log listen",
					Data:    logValidatorExample,
				},
			},
		},
		RequestProvider: func() interface{} {
			return &LogValidatorListenRequest{}
		},
		ResponseProvider: func() interface{} {
			return &LogValidatorListenResponse{}
		},
		Handler: func(context *Context, request interface{}) (interface{}, error) {
			if handlerRequest, ok := request.(*LogValidatorListenRequest); ok {
				return s.listen(context, handlerRequest)
			}
			return nil, fmt.Errorf("unsupported request type: %T", request)
		},
	})

	s.Register(&ServiceActionRoute{
		Action: "assert",
		RequestInfo: &ActionInfo{
			Description: "assert queued logs",
			Examples: []*ExampleUseCase{
				{
					UseCase: "assert",
					Data:    logValidatorAssertExample,
				},
			},
		},
		RequestProvider: func() interface{} {
			return &LogValidatorAssertRequest{}
		},
		ResponseProvider: func() interface{} {
			return &LogValidatorAssertResponse{}
		},
		Handler: func(context *Context, request interface{}) (interface{}, error) {
			if handlerRequest, ok := request.(*LogValidatorAssertRequest); ok {
				return s.assert(context, handlerRequest)
			}
			return nil, fmt.Errorf("unsupported request type: %T", request)
		},
	})

	s.Register(&ServiceActionRoute{
		Action: "reset",
		RequestInfo: &ActionInfo{
			Description: "reset logs queues",
		},
		RequestProvider: func() interface{} {
			return &LogValidatorResetRequest{}
		},
		ResponseProvider: func() interface{} {
			return &LogValidatorResetResponse{}
		},
		Handler: func(context *Context, request interface{}) (interface{}, error) {
			if handlerRequest, ok := request.(*LogValidatorResetRequest); ok {
				return s.reset(context, handlerRequest)
			}
			return nil, fmt.Errorf("unsupported request type: %T", request)
		},
	})
}

//NewLogValidatorService creates a new log validator service.
func NewLogValidatorService() Service {
	var result = &logValidatorService{
		AbstractService: NewAbstractService(LogValidatorServiceID),
	}
	result.AbstractService.Service = result
	result.registerRoutes()
	return result
}

func logTypeMetaKey(name string) string {
	return fmt.Sprintf("meta_%v", name)
}

//Next sets item pointer with next element.
func (i *logRecordIterator) Next(itemPointer interface{}) error {
	var indexRecordPointer, ok = itemPointer.(*IndexedLogRecord)
	if ok {
		logFile := i.logFiles[i.logFileIndex]
		logRecord := logFile.ShiftLogRecordByIndex(indexRecordPointer.IndexValue)
		indexRecordPointer.LogRecord = logRecord
		return nil
	}

	logRecordPointer, ok := itemPointer.(**LogRecord)
	if !ok {
		return fmt.Errorf("expected *%T buy had %T", &LogRecord{}, itemPointer)
	}
	logFile := i.logFiles[i.logFileIndex]
	logRecord := logFile.ShiftLogRecord()
	*logRecordPointer = logRecord
	return nil
}

//LogRecordIterator returns log record iterator
func (m *LogTypeMeta) LogRecordIterator() toolbox.Iterator {
	logFileProvider := func() []*LogFile {
		var result = make([]*LogFile, 0)
		for _, logFile := range m.LogFiles {
			result = append(result, logFile)
		}
		sort.Slice(result, func(i, j int) bool {
			var left = result[i].LastModified
			var right = result[j].LastModified
			if !left.After(right) && !right.After(left) {
				return result[i].URL > result[j].URL
			}
			return left.After(right)
		})
		return result
	}

	return &logRecordIterator{
		logFiles:        logFileProvider(),
		logFileProvider: logFileProvider,
	}
}

//NewLogTypeMeta creates a nre log type meta.
func NewLogTypeMeta(source *url.Resource, logType *LogType) *LogTypeMeta {
	return &LogTypeMeta{
		Source:   source,
		LogType:  logType,
		LogFiles: make(map[string]*LogFile),
	}
}

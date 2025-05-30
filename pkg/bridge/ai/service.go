package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	openai "github.com/sashabaranov/go-openai"
	"github.com/yomorun/yomo"
	"github.com/yomorun/yomo/ai"
	"github.com/yomorun/yomo/core/metadata"
	"github.com/yomorun/yomo/core/ylog"
	"github.com/yomorun/yomo/pkg/bridge/ai/provider"

	"github.com/yomorun/yomo/pkg/id"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Service is the  service layer for llm bridge server.
// service is responsible for handling the logic from handler layer.
type Service struct {
	provider      provider.LLMProvider
	newCallerFunc newCallerFunc
	callers       *expirable.LRU[string, *Caller]
	option        *ServiceOptions
	logger        *slog.Logger
}

// ServiceOptions is the option for creating service
type ServiceOptions struct {
	// Logger is the logger for the service
	Logger *slog.Logger
	// CredentialFunc is the function for getting the credential from the request
	CredentialFunc func(r *http.Request) (string, error)
	// CallerCacheSize is the size of the caller's cache
	CallerCacheSize int
	// CallerCacheTTL is the time to live of the callers cache
	CallerCacheTTL time.Duration
	// CallerCallTimeout is the timeout for awaiting the function response.
	CallerCallTimeout time.Duration
	// SourceBuilder should builds an unconnected source.
	SourceBuilder func(credential string) yomo.Source
	// ReducerBuilder should builds an unconnected reducer.
	ReducerBuilder func(credential string) yomo.StreamFunction
	// MetadataExchanger exchanges metadata from the credential.
	MetadataExchanger func(credential string) (metadata.M, error)
}

// NewService creates a new service for handling the logic from handler layer.
func NewService(provider provider.LLMProvider, opt *ServiceOptions) *Service {
	return NewServiceWithCallerFunc(provider, NewCaller, opt)
}

func initOption(opt *ServiceOptions) *ServiceOptions {
	if opt == nil {
		opt = &ServiceOptions{}
	}
	if opt.Logger == nil {
		opt.Logger = ylog.Default()
	}
	if opt.CredentialFunc == nil {
		opt.CredentialFunc = func(_ *http.Request) (string, error) { return "token", nil }
	}
	if opt.CallerCacheSize == 0 {
		opt.CallerCacheSize = 1
	}
	if opt.CallerCallTimeout == 0 {
		opt.CallerCallTimeout = 60 * time.Second
	}
	if opt.MetadataExchanger == nil {
		opt.MetadataExchanger = func(credential string) (metadata.M, error) {
			return metadata.New(), nil
		}
	}

	return opt
}

func NewServiceWithCallerFunc(provider provider.LLMProvider, ncf newCallerFunc, opt *ServiceOptions) *Service {
	onEvict := func(_ string, caller *Caller) {
		caller.Close()
	}

	opt = initOption(opt)

	service := &Service{
		provider:      provider,
		newCallerFunc: ncf,
		callers:       expirable.NewLRU(opt.CallerCacheSize, onEvict, opt.CallerCacheTTL),
		option:        opt,
		logger:        opt.Logger,
	}

	return service
}

type newCallerFunc func(yomo.Source, yomo.StreamFunction, metadata.M, time.Duration) (*Caller, error)

// LoadOrCreateCaller loads or creates the caller according to the http request.
func (srv *Service) LoadOrCreateCaller(r *http.Request) (*Caller, error) {
	credential, err := srv.option.CredentialFunc(r)
	if err != nil {
		return nil, err
	}
	return srv.loadOrCreateCaller(credential)
}

// GetInvoke returns the invoke response
func (srv *Service) GetInvoke(ctx context.Context, userInstruction, baseSystemMessage, transID string, caller *Caller, includeCallStack bool, tracer trace.Tracer) (*ai.InvokeResponse, error) {
	if tracer == nil {
		tracer = new(noop.Tracer)
	}
	md := caller.Metadata().Clone()
	// read tools attached to the metadata
	tools, err := ai.ListToolCalls(md)
	if err != nil {
		return &ai.InvokeResponse{}, err
	}
	chainMessage := ai.ChainMessage{}
	messages := srv.prepareMessages(baseSystemMessage, userInstruction, chainMessage, tools, true)
	req := openai.ChatCompletionRequest{
		Messages: messages,
	}
	// with tools
	if len(tools) > 0 {
		req.Tools = tools
	}
	var (
		promptUsage     int
		completionUsage int
	)
	_, span := tracer.Start(ctx, "first_call")
	chatCompletionResponse, err := srv.provider.GetChatCompletions(ctx, req, md)
	if err != nil {
		return nil, err
	}
	promptUsage = chatCompletionResponse.Usage.PromptTokens
	completionUsage = chatCompletionResponse.Usage.CompletionTokens

	// convert ChatCompletionResponse to InvokeResponse
	res, err := ai.ConvertToInvokeResponse(&chatCompletionResponse, tools)
	if err != nil {
		return nil, err
	}
	// if no tool_calls fired, just return the llm text result
	if res.FinishReason != string(openai.FinishReasonToolCalls) {
		return res, nil
	}
	span.End()

	// run llm function calls
	srv.logger.Debug(">>>> start 1st call response",
		"res_toolcalls", fmt.Sprintf("%+v", res.ToolCalls),
		"res_assistant_msgs", fmt.Sprintf("%+v", res.AssistantMessage))

	srv.logger.Debug(">> run function calls", "transID", transID, "res.ToolCalls", fmt.Sprintf("%+v", res.ToolCalls))

	sfnCtx, span := tracer.Start(ctx, "run_sfn")
	reqID := id.New(16)
	callResult, err := caller.Call(sfnCtx, transID, reqID, res.ToolCalls, tracer)
	if err != nil {
		return nil, err
	}
	span.End()

	srv.logger.Debug(">>>> start 2nd call with", "calls", fmt.Sprintf("%+v", callResult), "preceeding_assistant_message", fmt.Sprintf("%+v", res.AssistantMessage))

	chainMessage.PreceedingAssistantMessage = res.AssistantMessage
	llmCalls := make([]openai.ChatCompletionMessage, len(callResult))
	for k, v := range callResult {
		llmCalls[k] = openai.ChatCompletionMessage{
			ToolCallID: v.ToolCallID,
			Role:       openai.ChatMessageRoleTool,
			Content:    v.Content,
		}
	}
	chainMessage.ToolMessages = transToolMessage(llmCalls)
	// do not attach toolMessage to prompt in 2nd call
	messages2 := srv.prepareMessages(baseSystemMessage, userInstruction, chainMessage, tools, false)
	req2 := openai.ChatCompletionRequest{
		Messages: messages2,
	}
	_, span = tracer.Start(ctx, "second_call")
	chatCompletionResponse2, err := srv.provider.GetChatCompletions(ctx, req2, md)
	if err != nil {
		return nil, err
	}
	span.End()

	chatCompletionResponse2.Usage.PromptTokens += promptUsage
	chatCompletionResponse2.Usage.CompletionTokens += completionUsage

	res2, err := ai.ConvertToInvokeResponse(&chatCompletionResponse2, tools)
	if err != nil {
		return nil, err
	}

	// INFO: call stack infomation
	if includeCallStack {
		res2.ToolCalls = res.ToolCalls
		res2.ToolMessages = transToolMessage(llmCalls)
	}
	srv.logger.Debug("<<<< complete 2nd call", "res2", fmt.Sprintf("%+v", res2))

	return res2, err
}

// GetChatCompletions accepts openai.ChatCompletionRequest and responds to http.ResponseWriter.
func (srv *Service) GetChatCompletions(ctx context.Context, req openai.ChatCompletionRequest, transID string, caller *Caller, w EventResponseWriter, tracer trace.Tracer) error {
	if tracer == nil {
		tracer = new(noop.Tracer)
	}
	reqCtx, reqSpan := tracer.Start(ctx, "completions_request")
	md := caller.Metadata().Clone()

	// 1. find all hosting tool sfn
	tools, err := ai.ListToolCalls(md)
	if err != nil {
		return err
	}
	// 2. add those tools to request
	req, hasReqTools := srv.addToolsToRequest(req, tools)

	// 3. operate system prompt to request
	prompt, op := caller.GetSystemPrompt()
	req = srv.OpSystemPrompt(req, prompt, op)

	var (
		promptUsage      = 0
		completionUsage  = 0
		totalUsage       = 0
		reqMessages      = req.Messages
		toolCallsMap     = make(map[int]openai.ToolCall)
		toolCalls        = []openai.ToolCall{}
		assistantMessage = openai.ChatCompletionMessage{}
	)

	// 4. request first chat for getting tools
	if req.Stream {
		w.RecordIsStream(true)
		_, firstCallSpan := tracer.Start(reqCtx, "first_call_request")

		resStream, err := srv.provider.GetChatCompletionsStream(reqCtx, req, md)
		if err != nil {
			return err
		}

		w.SetStreamHeader()

		var (
			isFunctionCall = false
			i              int // number of chunks
			j              int // number of tool call chunks
			firstRespSpan  trace.Span
			respSpan       trace.Span
		)
		for {
			if i == 0 {
				_, firstRespSpan = tracer.Start(reqCtx, "first_call_response_in_stream")
			}
			streamRes, err := resStream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if hasReqTools {
				if i == 0 {
					respSpan = startRespSpan(reqCtx, reqSpan, tracer, w)
				}
				w.WriteStreamEvent(streamRes)
				i++
				continue
			}
			if len(streamRes.PromptFilterResults) > 0 {
				continue
			}

			if streamRes.Usage != nil {
				promptUsage = streamRes.Usage.PromptTokens
				completionUsage = streamRes.Usage.CompletionTokens
				totalUsage = streamRes.Usage.TotalTokens
			}

			choices := streamRes.Choices
			if len(choices) > 0 && len(choices[0].Delta.ToolCalls) > 0 {
				tc := choices[0].Delta.ToolCalls
				isFunctionCall = true
				if j == 0 {
					firstCallSpan.End()
				}
				for _, t := range tc {
					// this index should be toolCalls slice's index, the index field only appares in stream response
					index := *t.Index
					item, ok := toolCallsMap[index]
					if !ok {
						toolCallsMap[index] = openai.ToolCall{
							Index:    t.Index,
							ID:       t.ID,
							Type:     t.Type,
							Function: openai.FunctionCall{},
						}
						item = toolCallsMap[index]
					}
					if t.Function.Arguments != "" {
						item.Function.Arguments += t.Function.Arguments
					}
					if t.Function.Name != "" {
						item.Function.Name = t.Function.Name
					}
					toolCallsMap[index] = item
				}
				j++
			} else if !isFunctionCall {
				_ = w.WriteStreamEvent(streamRes)
			}
			if i == 0 && j == 0 && !isFunctionCall {
				respSpan = startRespSpan(reqCtx, reqSpan, tracer, w)
			}
			i++
		}
		if !isFunctionCall || hasReqTools {
			respSpan.End()
			return w.WriteStreamDone()
		}
		firstRespSpan.End()
		toolCalls = mapToSliceTools(toolCallsMap)

		assistantMessage = openai.ChatCompletionMessage{
			ToolCalls: toolCalls,
			Role:      openai.ChatMessageRoleAssistant,
		}
		reqSpan.End()
		w.Flush() // flush the header before write body to the client.
	} else {
		_, firstCallSpan := tracer.Start(reqCtx, "first_call")
		resp, err := srv.provider.GetChatCompletions(ctx, req, md)
		if err != nil {
			return err
		}

		promptUsage = resp.Usage.PromptTokens
		completionUsage = resp.Usage.CompletionTokens
		totalUsage = resp.Usage.CompletionTokens

		srv.logger.Debug(" #1 first call", "response", fmt.Sprintf("%+v", resp))
		// it is a function call
		if resp.Choices[0].FinishReason == openai.FinishReasonToolCalls && !hasReqTools {
			toolCalls = append(toolCalls, resp.Choices[0].Message.ToolCalls...)
			assistantMessage = resp.Choices[0].Message
			firstCallSpan.End()
			reqSpan.End()
		} else {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return nil
		}
	}

	resCtx, resSpan := tracer.Start(ctx, "completions_response")
	defer resSpan.End()

	sfnCtx, sfnSpan := tracer.Start(resCtx, "run_sfn")

	// 5. find sfns that hit the function call
	fnCalls := findTools(tools, toolCalls)

	_ = w.WriteStreamEvent(toolCalls)

	// 6. run llm function calls
	reqID := id.New(16)
	callResult, err := caller.Call(sfnCtx, transID, reqID, fnCalls, tracer)
	if err != nil {
		return err
	}
	_ = w.WriteStreamEvent(callResult)
	sfnSpan.End()

	// 7. do the second call (the second call messages are from user input, first call resopnse and sfn calls result)
	llmCalls := make([]openai.ChatCompletionMessage, len(callResult))
	for k, v := range callResult {
		llmCalls[k] = openai.ChatCompletionMessage{
			ToolCallID: v.ToolCallID,
			Role:       openai.ChatMessageRoleTool,
			Content:    v.Content,
		}
	}
	// second call should not have tool_choice option
	req.ToolChoice = nil
	req.Messages = append(reqMessages, assistantMessage)
	req.Messages = append(req.Messages, llmCalls...)
	// anthropic must define tools
	if srv.provider.Name() != "anthropic" {
		req.Tools = nil // reset tools field
	}

	srv.logger.Debug(" #2 second call", "request", fmt.Sprintf("%+v", req))

	if req.Stream {
		_, secondCallSpan := tracer.Start(resCtx, "second_call_request")
		resStream, err := srv.provider.GetChatCompletionsStream(resCtx, req, md)
		if err != nil {
			return err
		}
		secondCallSpan.End()

		var (
			i              int
			secondRespSpan trace.Span
		)
		for {
			if i == 0 {
				_, secondRespSpan = tracer.Start(resCtx, "second_call_response_in_stream(TBT)")
			}
			i++
			streamRes, err := resStream.Recv()
			if err == io.EOF {
				secondRespSpan.End()
				return w.WriteStreamDone()
			}
			if err != nil {
				return err
			}
			if streamRes.Usage != nil {
				streamRes.Usage.PromptTokens += promptUsage
				streamRes.Usage.CompletionTokens += completionUsage
				streamRes.Usage.TotalTokens += totalUsage
			}
			_ = w.WriteStreamEvent(streamRes)
		}
	} else {
		_, secondCallSpan := tracer.Start(resCtx, "second_call")

		resp, err := srv.provider.GetChatCompletions(resCtx, req, md)
		if err != nil {
			return err
		}

		resp.Usage.PromptTokens += promptUsage
		resp.Usage.CompletionTokens += completionUsage
		resp.Usage.TotalTokens += totalUsage

		secondCallSpan.End()

		srv.logger.Debug(" #2 second call", "response", fmt.Sprintf("%+v", resp))
		w.Header().Set("Content-Type", "application/json")
		return json.NewEncoder(w).Encode(resp)
	}
}

// Logger returns the logger of the service
func (src *Service) Logger() *slog.Logger {
	return src.logger
}

func startRespSpan(ctx context.Context, reqSpan trace.Span, tracer trace.Tracer, w EventResponseWriter) trace.Span {
	reqSpan.End()
	recordTTFT(ctx, tracer, w)
	_, respSpan := tracer.Start(ctx, "response_in_stream(TBT)")
	return respSpan
}

func (srv *Service) loadOrCreateCaller(credential string) (*Caller, error) {
	caller, ok := srv.callers.Get(credential)
	if ok {
		return caller, nil
	}
	md, err := srv.option.MetadataExchanger(credential)
	if err != nil {
		return nil, err
	}
	caller, err = srv.newCallerFunc(
		srv.option.SourceBuilder(credential),
		srv.option.ReducerBuilder(credential),
		md,
		srv.option.CallerCallTimeout,
	)
	if err != nil {
		return nil, err
	}

	srv.callers.Add(credential, caller)

	return caller, nil
}

func (srv *Service) addToolsToRequest(req openai.ChatCompletionRequest, tools []openai.Tool) (openai.ChatCompletionRequest, bool) {
	hasReqTools := len(req.Tools) > 0
	if !hasReqTools {
		if len(tools) > 0 {
			req.Tools = tools
			srv.logger.Debug("#1 first call", "request", fmt.Sprintf("%+v", req))
		}
	}
	return req, hasReqTools
}

func (srv *Service) OpSystemPrompt(req openai.ChatCompletionRequest, sysPrompt string, op SystemPromptOp) openai.ChatCompletionRequest {
	if op == SystemPromptOpDisabled {
		return req
	}
	if op == SystemPromptOpOverwrite && sysPrompt == "" {
		return req
	}
	var (
		systemCount = 0
		messages    = []openai.ChatCompletionMessage{}
	)
	for _, msg := range req.Messages {
		if msg.Role != "system" {
			messages = append(messages, msg)
			continue
		}
		if systemCount == 0 {
			content := ""
			switch op {
			case SystemPromptOpPrefix:
				content = sysPrompt + "\n" + msg.Content
			case SystemPromptOpOverwrite:
				content = sysPrompt
			}
			messages = append(messages, openai.ChatCompletionMessage{
				Role:    msg.Role,
				Content: content,
			})
		}
		systemCount++
	}

	if systemCount == 0 && sysPrompt != "" {
		message := openai.ChatCompletionMessage{
			Role:    "system",
			Content: sysPrompt,
		}
		messages = append([]openai.ChatCompletionMessage{message}, req.Messages...)
	}
	req.Messages = messages

	srv.logger.Debug(" #1 first call after operating", "request", fmt.Sprintf("%+v", req))

	return req
}

func findTools(tools []openai.Tool, toolCalls []openai.ToolCall) []openai.ToolCall {
	fnCalls := []openai.ToolCall{}

	for _, call := range toolCalls {
		for _, tool := range tools {
			if tool.Function.Name == call.Function.Name && tool.Type == call.Type {
				fnCalls = append(fnCalls, call)
			}
		}
	}
	return fnCalls
}

func (srv *Service) prepareMessages(baseSystemMessage string, userInstruction string, chainMessage ai.ChainMessage, tools []openai.Tool, withTool bool) []openai.ChatCompletionMessage {
	systemInstructions := []string{"## Instructions\n"}

	// only append if there are tool calls
	if withTool {
		for _, t := range tools {
			systemInstructions = append(systemInstructions, "- ")
			systemInstructions = append(systemInstructions, t.Function.Description)
			systemInstructions = append(systemInstructions, "\n")
		}
		systemInstructions = append(systemInstructions, "\n")
	}

	SystemPrompt := fmt.Sprintf("%s\n\n%s", baseSystemMessage, strings.Join(systemInstructions, ""))

	messages := []openai.ChatCompletionMessage{}

	// 1. system message
	messages = append(messages, openai.ChatCompletionMessage{Role: "system", Content: SystemPrompt})

	// 2. previous tool calls
	// Ref: Tool Message Object in Messsages
	// https://platform.openai.com/docs/guides/function-calling
	// https://platform.openai.com/docs/api-reference/chat/create#chat-create-messages

	if chainMessage.PreceedingAssistantMessage != nil {
		// 2.1 assistant message
		// try convert type of chainMessage.PreceedingAssistantMessage to type ChatCompletionMessage
		assistantMessage, ok := chainMessage.PreceedingAssistantMessage.(openai.ChatCompletionMessage)
		if ok {
			srv.logger.Debug("======== add assistantMessage", "am", fmt.Sprintf("%+v", assistantMessage))
			messages = append(messages, assistantMessage)
		}

		// 2.2 tool message
		for _, tool := range chainMessage.ToolMessages {
			tm := openai.ChatCompletionMessage{
				Role:       "tool",
				Content:    tool.Content,
				ToolCallID: tool.ToolCallID,
			}
			srv.logger.Debug("======== add toolMessage", "tm", fmt.Sprintf("%+v", tm))
			messages = append(messages, tm)
		}
	}

	// 3. user instruction
	messages = append(messages, openai.ChatCompletionMessage{Role: "user", Content: userInstruction})

	return messages
}

func mapToSliceTools(m map[int]openai.ToolCall) []openai.ToolCall {
	arr := make([]openai.ToolCall, len(m))
	for k, v := range m {
		arr[k] = v
	}
	return arr
}

func transToolMessage(msgs []openai.ChatCompletionMessage) []ai.ToolMessage {
	toolMessages := make([]ai.ToolMessage, len(msgs))
	for i, msg := range msgs {
		toolMessages[i] = ai.ToolMessage{
			Role:       msg.Role,
			Content:    msg.Content,
			ToolCallID: msg.ToolCallID,
		}
	}
	return toolMessages
}

func recordTTFT(ctx context.Context, tracer trace.Tracer, w EventResponseWriter) {
	_, span := tracer.Start(ctx, "TTFT")
	time.Sleep(time.Millisecond)
	span.End()
	w.RecordTTFT(time.Now())
	time.Sleep(time.Millisecond)
}

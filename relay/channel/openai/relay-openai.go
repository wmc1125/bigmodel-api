package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"one-api/common"
	"one-api/constant"
	"one-api/dto"
	relaycommon "one-api/relay/common"
	relayconstant "one-api/relay/constant"
	"one-api/service"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

// 处理 OpenAI 流式响应
func OaiStreamHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*dto.OpenAIErrorWithStatusCode, *dto.Usage) {
	containStreamUsage := false
	var responseId string
	var createAt int64 = 0
	var systemFingerprint string
	model := info.UpstreamModelName

	var responseTextBuilder strings.Builder
	var usage = &dto.Usage{}
	var streamItems []string // 存储流项目

	toolCount := 0
	scanner := bufio.NewScanner(resp.Body)
	scanner.Split(bufio.ScanLines)

	service.SetEventStreamHeaders(c)

	ticker := time.NewTicker(time.Duration(constant.StreamingTimeout) * time.Second)
	defer ticker.Stop()

	stopChan := make(chan bool)
	defer close(stopChan)
	var (
		lastStreamData string
		mu             sync.Mutex
	)
	gopool.Go(func() {
		for scanner.Scan() {
			info.SetFirstResponseTime()
			ticker.Reset(time.Duration(constant.StreamingTimeout) * time.Second)
			data := scanner.Text()
			if len(data) < 6 { // 忽略空行或格式错误的行
				continue
			}
			if data[:6] != "data: " && data[:6] != "[DONE]" {
				continue
			}
			mu.Lock()
			data = data[6:]
			if !strings.HasPrefix(data, "[DONE]") {
				if lastStreamData != "" {
					err := service.StringData(c, lastStreamData)
					if err != nil {
						common.LogError(c, "streaming error: "+err.Error())
					}
				}
				lastStreamData = data
				streamItems = append(streamItems, data)
			}
			mu.Unlock()
		}

		// 根据分组加小尾巴
		if _, ok := common.UserUsableGroupChatTails[info.Group]; ok {
			group_tag := common.UserUsableGroupChatTails[info.Group]
			// -----------增加尾巴--------
			endMessage := `{"id":"chatcmpl-end","object":"chat.completion.chunk","created":` + fmt.Sprint(time.Now().Unix()) + `,"model":"` + model + `","choices":[{"index":0,"delta":{"content":"` + group_tag + `"},"finish_reason":"stop"}]}`
			err := service.StringData(c, endMessage)
			if err != nil {
				common.LogError(c, "failed to write endMessage: "+err.Error())
			}
			// 确保数据被立即发送
			if flusher, ok := c.Writer.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		// -----------增加尾巴 end--------

		common.SafeSendBool(stopChan, true)
	})

	select {
	case <-ticker.C:
		// 超时处理逻辑
		common.LogError(c, "streaming timeout")
	case <-stopChan:
		// 正常结束
	}

	shouldSendLastResp := true
	var lastStreamResponse dto.ChatCompletionsStreamResponse
	err := json.Unmarshal(common.StringToByteSlice(lastStreamData), &lastStreamResponse)
	if err == nil {
		responseId = lastStreamResponse.Id
		createAt = lastStreamResponse.Created
		systemFingerprint = lastStreamResponse.GetSystemFingerprint()
		model = lastStreamResponse.Model
		if service.ValidUsage(lastStreamResponse.Usage) {
			containStreamUsage = true
			usage = lastStreamResponse.Usage
			if !info.ShouldIncludeUsage {
				shouldSendLastResp = false
			}
		}
	}
	if shouldSendLastResp {
		service.StringData(c, lastStreamData)
	}

	// 计算token
	streamResp := "[" + strings.Join(streamItems, ",") + "]"
	switch info.RelayMode {
	case relayconstant.RelayModeChatCompletions:
		var streamResponses []dto.ChatCompletionsStreamResponse
		err := json.Unmarshal(common.StringToByteSlice(streamResp), &streamResponses)
		if err != nil {
			// 一次性解析失败，逐个解析
			common.SysError("error unmarshalling stream response: " + err.Error())
			for _, item := range streamItems {
				var streamResponse dto.ChatCompletionsStreamResponse
				err := json.Unmarshal(common.StringToByteSlice(item), &streamResponse)
				if err == nil {
					//if service.ValidUsage(streamResponse.Usage) {
					//	usage = streamResponse.Usage
					//}
					for _, choice := range streamResponse.Choices {
						responseTextBuilder.WriteString(choice.Delta.GetContentString())
						if choice.Delta.ToolCalls != nil {
							if len(choice.Delta.ToolCalls) > toolCount {
								toolCount = len(choice.Delta.ToolCalls)
							}
							for _, tool := range choice.Delta.ToolCalls {
								responseTextBuilder.WriteString(tool.Function.Name)
								responseTextBuilder.WriteString(tool.Function.Arguments)
							}
						}
					}
				}
			}
		} else {
			for _, streamResponse := range streamResponses {
				//if service.ValidUsage(streamResponse.Usage) {
				//	usage = streamResponse.Usage
				//	containStreamUsage = true
				//}
				for _, choice := range streamResponse.Choices {
					responseTextBuilder.WriteString(choice.Delta.GetContentString())
					if choice.Delta.ToolCalls != nil {
						if len(choice.Delta.ToolCalls) > toolCount {
							toolCount = len(choice.Delta.ToolCalls)
						}
						for _, tool := range choice.Delta.ToolCalls {
							responseTextBuilder.WriteString(tool.Function.Name)
							responseTextBuilder.WriteString(tool.Function.Arguments)
						}
					}
				}
			}
		}
	case relayconstant.RelayModeCompletions:
		var streamResponses []dto.CompletionsStreamResponse
		err := json.Unmarshal(common.StringToByteSlice(streamResp), &streamResponses)
		if err != nil {
			// 一次性解析失败，逐个解析
			common.SysError("error unmarshalling stream response: " + err.Error())
			for _, item := range streamItems {
				var streamResponse dto.CompletionsStreamResponse
				err := json.Unmarshal(common.StringToByteSlice(item), &streamResponse)
				if err == nil {
					for _, choice := range streamResponse.Choices {
						responseTextBuilder.WriteString(choice.Text)
					}
				}
			}
		} else {
			for _, streamResponse := range streamResponses {
				for _, choice := range streamResponse.Choices {
					responseTextBuilder.WriteString(choice.Text)
				}
			}
		}
	}

	if !containStreamUsage {
		usage, _ = service.ResponseText2Usage(responseTextBuilder.String(), info.UpstreamModelName, info.PromptTokens)
		usage.CompletionTokens += toolCount * 7
	}

	if info.ShouldIncludeUsage && !containStreamUsage {
		response := service.GenerateFinalUsageResponse(responseId, createAt, model, *usage)
		response.SetSystemFingerprint(systemFingerprint)
		service.ObjectData(c, response)
	}

	service.Done(c)

	resp.Body.Close()
	return nil, usage
}

// 处理 OpenAI 普通响应
func OpenaiHandler(c *gin.Context, resp *http.Response, promptTokens int, model string) (*dto.OpenAIErrorWithStatusCode, *dto.Usage) {
	var simpleResponse dto.SimpleResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return service.OpenAIErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError), nil
	}
	err = resp.Body.Close()
	if err != nil {
		return service.OpenAIErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil
	}
	err = json.Unmarshal(responseBody, &simpleResponse)
	if err != nil {
		return service.OpenAIErrorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError), nil
	}
	if simpleResponse.Error.Type != "" {
		return &dto.OpenAIErrorWithStatusCode{
			Error:      simpleResponse.Error,
			StatusCode: resp.StatusCode,
		}, nil
	}

	// 在发送响应之前添加小尾巴
	if group, ok := c.Get("group"); ok {
		if groupTag, exists := common.UserUsableGroupChatTails[group.(string)]; exists {
			// 将小尾巴添加到响应中
			var responseData map[string]interface{}
			err = json.Unmarshal(responseBody, &responseData)
			if err == nil {
				if choices, ok := responseData["choices"].([]interface{}); ok && len(choices) > 0 {
					if choice, ok := choices[0].(map[string]interface{}); ok {
						if message, ok := choice["message"].(map[string]interface{}); ok {
							content, _ := message["content"].(string)
							message["content"] = content + groupTag
						}
					}
				}
				// 重新编码修改后的响应
				modifiedResponseBody, err := json.Marshal(responseData)
				if err == nil {
					responseBody = modifiedResponseBody
				}
			}
		}
	}

	// 重置响应体
	resp.Body = io.NopCloser(bytes.NewBuffer(responseBody))
	// 在解析响应体之前不应设置头部，因为解析部分可能会失败。
	// 然后我们将不得不发送错误响应，但在这种情况下，头部已经设置。
	// 因此，httpClient 会对响应感到困惑。
	// 例如，Postman 会报告错误，我们无法检查响应。
	for k, v := range resp.Header {
		c.Writer.Header().Set(k, v[0])
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = io.Copy(c.Writer, resp.Body)
	if err != nil {
		return service.OpenAIErrorWrapper(err, "copy_response_body_failed", http.StatusInternalServerError), nil
	}
	resp.Body.Close()
	if simpleResponse.Usage.TotalTokens == 0 || (simpleResponse.Usage.PromptTokens == 0 && simpleResponse.Usage.CompletionTokens == 0) {
		completionTokens := 0
		for _, choice := range simpleResponse.Choices {
			ctkm, _ := service.CountTokenText(string(choice.Message.Content), model)
			completionTokens += ctkm
		}
		simpleResponse.Usage = dto.Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		}
	}
	return nil, &simpleResponse.Usage
}

// 处理 OpenAI 语音合成响应
func OpenaiTTSHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*dto.OpenAIErrorWithStatusCode, *dto.Usage) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return service.OpenAIErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError), nil
	}
	err = resp.Body.Close()
	if err != nil {
		return service.OpenAIErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil
	}
	// 重置响应体
	resp.Body = io.NopCloser(bytes.NewBuffer(responseBody))
	// 在解析响应体之前不应设置头部，因为解析部分可能会失败。
	// 然后我们将不得不发送错误响应，但在这种情况下，头部已经设置。
	// 因此，httpClient 会对响应感到困惑。
	// 例如，Postman 会报告错误，我们无法检查响应。
	for k, v := range resp.Header {
		c.Writer.Header().Set(k, v[0])
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = io.Copy(c.Writer, resp.Body)
	if err != nil {
		return service.OpenAIErrorWrapper(err, "copy_response_body_failed", http.StatusInternalServerError), nil
	}
	err = resp.Body.Close()
	if err != nil {
		return service.OpenAIErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil
	}

	usage := &dto.Usage{}
	usage.PromptTokens = info.PromptTokens
	usage.TotalTokens = info.PromptTokens
	return nil, usage
}

// 处理 OpenAI 语音识别响应
func OpenaiSTTHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo, responseFormat string) (*dto.OpenAIErrorWithStatusCode, *dto.Usage) {
	var audioResp dto.AudioResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return service.OpenAIErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError), nil
	}
	err = resp.Body.Close()
	if err != nil {
		return service.OpenAIErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil
	}
	err = json.Unmarshal(responseBody, &audioResp)
	if err != nil {
		return service.OpenAIErrorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError), nil
	}

	// 重置响应体
	resp.Body = io.NopCloser(bytes.NewBuffer(responseBody))
	// 在解析响应体之前不应设置头部，因为解析部分可能会失败。
	// 然后我们将不得不发送错误响应，但在这种情况下，头部已经设置。
	// 因此，httpClient 会对响应感到困惑。
	// 例如，Postman 会报告错误，我们无法检查响应。
	for k, v := range resp.Header {
		c.Writer.Header().Set(k, v[0])
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = io.Copy(c.Writer, resp.Body)
	if err != nil {
		return service.OpenAIErrorWrapper(err, "copy_response_body_failed", http.StatusInternalServerError), nil
	}
	resp.Body.Close()

	var text string
	switch responseFormat {
	case "json":
		text, err = getTextFromJSON(responseBody)
	case "text":
		text, err = getTextFromText(responseBody)
	case "srt":
		text, err = getTextFromSRT(responseBody)
	case "verbose_json":
		text, err = getTextFromVerboseJSON(responseBody)
	case "vtt":
		text, err = getTextFromVTT(responseBody)
	}

	usage := &dto.Usage{}
	usage.PromptTokens = info.PromptTokens
	usage.CompletionTokens, _ = service.CountTokenText(text, info.UpstreamModelName)
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	return nil, usage
}

// 从 VTT 格式中提取文本
func getTextFromVTT(body []byte) (string, error) {
	return getTextFromSRT(body)
}

// 从详细 JSON 格式中提取文本
func getTextFromVerboseJSON(body []byte) (string, error) {
	var whisperResponse dto.WhisperVerboseJSONResponse
	if err := json.Unmarshal(body, &whisperResponse); err != nil {
		return "", fmt.Errorf("unmarshal_response_body_failed err :%w", err)
	}
	return whisperResponse.Text, nil
}

// 从 SRT 格式中提取文本
func getTextFromSRT(body []byte) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	var builder strings.Builder
	var textLine bool
	for scanner.Scan() {
		line := scanner.Text()
		if textLine {
			builder.WriteString(line)
			textLine = false
			continue
		} else if strings.Contains(line, "-->") {
			textLine = true
			continue
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return builder.String(), nil
}

// 从纯文本格式中提取文本
func getTextFromText(body []byte) (string, error) {
	return strings.TrimSuffix(string(body), "\n"), nil
}

// 从 JSON 格式中提取文本
func getTextFromJSON(body []byte) (string, error) {
	var whisperResponse dto.AudioResponse
	if err := json.Unmarshal(body, &whisperResponse); err != nil {
		return "", fmt.Errorf("unmarshal_response_body_failed err :%w", err)
	}
	return whisperResponse.Text, nil
}

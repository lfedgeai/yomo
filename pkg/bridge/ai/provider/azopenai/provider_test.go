package azopenai

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
)

func TestNewProvider(t *testing.T) {
	// Set environment variables for testing
	os.Setenv("AZURE_OPENAI_API_KEY", "test_api_key")
	os.Setenv("AZURE_OPENAI_API_ENDPOINT", "test_api_endpoint")
	os.Setenv("AZURE_OPENAI_DEPLOYMENT_ID", "test_deployment_id")
	os.Setenv("AZURE_OPENAI_API_VERSION", "test_api_version")

	provider := NewProvider("", "", "", "")

	assert.Equal(t, "test_api_key", provider.APIKey)
	assert.Equal(t, "test_api_endpoint", provider.APIEndpoint)
	assert.Equal(t, "test_deployment_id", provider.DeploymentID)
	assert.Equal(t, "test_api_version", provider.APIVersion)
}

func TestAzureOpenAIProvider_Name(t *testing.T) {
	provider := &Provider{}
	name := provider.Name()

	assert.Equal(t, "azopenai", name)
}

func TestAzureOpenAIProvider_GetChatCompletions(t *testing.T) {
	config := newConfig("test", "https://yomo.openai.azure.com", "test", "test-version")
	config.HTTPClient = &http.Client{Timeout: time.Millisecond}

	provider := &Provider{
		APIKey:       "test",
		APIEndpoint:  "https://yomo.openai.azure.com",
		DeploymentID: "test",
		APIVersion:   "test-version",
		client:       openai.NewClientWithConfig(config),
	}
	msgs := []openai.ChatCompletionMessage{
		{
			Role:    "user",
			Content: "hello",
		},
		{
			Role:    "system",
			Content: "I'm a bot",
		},
	}
	req := openai.ChatCompletionRequest{
		Model:    "gp-3.5-turbo",
		Messages: msgs,
	}

	_, err := provider.GetChatCompletions(context.TODO(), req, nil)
	t.Log(err)

	_, err = provider.GetChatCompletionsStream(context.TODO(), req, nil)
	t.Log(err)
}

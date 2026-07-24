package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// bedrockModelID is the Claude Haiku 4.5 cross-region inference profile. Confirm
// the exact string at build with: aws bedrock list-inference-profiles --region us-east-1
// | grep -i haiku   (us-east-1 → "us." prefix; ca-central-1 may need its own profile).
const bedrockModelID = "us.anthropic.claude-haiku-4-5-20251001-v1:0"
const llmMaxTokens = 150
const llmTemperature = 0.8

const defaultSystemPrompt = "You are a helpful assistant on a mesh network. Keep responses under 230 characters."

// generateReply routes to the Anthropic first-party API when MESHTK_ANTHROPIC_KEY
// is set (operator-flippable backup), otherwise to Amazon Bedrock (task-role auth,
// the prod default). OpenAI has been removed.
func generateReply(ctx context.Context, message, systemPrompt string) (string, error) {
	if systemPrompt == "" {
		systemPrompt = defaultSystemPrompt
	}
	if key := os.Getenv("MESHTK_ANTHROPIC_KEY"); key != "" {
		return callClaudeAnthropic(ctx, message, systemPrompt, key)
	}
	return callClaudeBedrock(ctx, message, systemPrompt)
}

func callClaudeBedrock(ctx context.Context, message, systemPrompt string) (string, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("aws config: %w", err)
	}
	client := bedrockruntime.NewFromConfig(cfg)
	out, err := client.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId: aws.String(bedrockModelID),
		System:  []brtypes.SystemContentBlock{&brtypes.SystemContentBlockMemberText{Value: systemPrompt}},
		Messages: []brtypes.Message{{
			Role:    brtypes.ConversationRoleUser,
			Content: []brtypes.ContentBlock{&brtypes.ContentBlockMemberText{Value: message}},
		}},
		InferenceConfig: &brtypes.InferenceConfiguration{
			MaxTokens:   aws.Int32(llmMaxTokens),
			Temperature: aws.Float32(llmTemperature),
		},
	})
	if err != nil {
		return "", fmt.Errorf("bedrock converse: %w", err)
	}
	msg, ok := out.Output.(*brtypes.ConverseOutputMemberMessage)
	if !ok {
		return "", fmt.Errorf("bedrock: unexpected output type")
	}
	var sb bytes.Buffer
	for _, c := range msg.Value.Content {
		if t, ok := c.(*brtypes.ContentBlockMemberText); ok {
			sb.WriteString(t.Value)
		}
	}
	return sb.String(), nil
}

func anthropicModelBody(model, system, message string, maxTokens int, temp float64) []byte {
	body, _ := json.Marshal(map[string]any{
		"model":       model,
		"system":      system,
		"max_tokens":  maxTokens,
		"temperature": temp,
		"messages":    []map[string]string{{"role": "user", "content": message}},
	})
	return body
}

func parseAnthropicResponse(body []byte) (string, error) {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("anthropic decode: %w", err)
	}
	if r.Error.Message != "" {
		return "", fmt.Errorf("anthropic: %s", r.Error.Message)
	}
	var sb bytes.Buffer
	for _, c := range r.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), nil
}

func callClaudeAnthropic(ctx context.Context, message, systemPrompt, key string) (string, error) {
	body := anthropicModelBody("claude-haiku-4-5", systemPrompt, message, llmMaxTokens, llmTemperature)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic request: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anthropic status %d: %s", resp.StatusCode, string(b))
	}
	return parseAnthropicResponse(b)
}

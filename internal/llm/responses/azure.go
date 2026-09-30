package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"scode/internal/llm"
)

// AzureProvider streams assistant turns over Azure OpenAI's Responses
// endpoint (pi's azure-openai-responses.ts): the Responses protocol with
// api-key auth, a deployment name in the model field, and an api-version
// query parameter.
type AzureProvider struct {
	Key        string
	BaseURL    string // overrides env resolution when set (pi's azureBaseUrl option)
	APIVersion string // overrides AZURE_OPENAI_API_VERSION when set
	HTTP       *http.Client
	// Transport carries pi's settings.retry.provider knobs.
	Transport llm.TransportConfig
}

// DefaultAzureAPIVersion is pi's DEFAULT_AZURE_API_VERSION.
const DefaultAzureAPIVersion = "v1"

// azureToolCallProviders is pi's AZURE_TOOL_CALL_PROVIDERS mapped onto
// scode's provider registry.
var azureToolCallProviders = map[string]bool{
	"openai-responses":       true,
	"azure-openai-responses": true,
}

func NewAzure(key, baseURL string) *AzureProvider {
	return &AzureProvider{Key: key, BaseURL: baseURL, HTTP: http.DefaultClient}
}

func (p *AzureProvider) Name() string { return "azure-openai-responses" }

func (p *AzureProvider) Caps() llm.Capabilities {
	return llm.Capabilities{
		// Azure sends prompt_cache_key but no retention parameter.
		StreamingToolArguments: true,
	}
}

func (p *AzureProvider) transport() llm.TransportConfig { return p.Transport }

func (p *AzureProvider) resolveKey(opts llm.StreamOptions) string {
	if opts.APIKey != "" {
		return opts.APIKey
	}
	if p.Key != "" {
		return p.Key
	}
	return os.Getenv("AZURE_OPENAI_API_KEY")
}

// resolveDeploymentName maps the catalog model id to the Azure deployment
// (pi's resolveDeploymentName): AZURE_OPENAI_DEPLOYMENT_NAME_MAP entries
// ("model=deployment,...") win; otherwise the model id IS the deployment.
func resolveDeploymentName(modelID string) string {
	for _, entry := range strings.Split(os.Getenv("AZURE_OPENAI_DEPLOYMENT_NAME_MAP"), ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, dep, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(id) == modelID {
			return strings.TrimSpace(dep)
		}
	}
	return modelID
}

// resolveBaseURL is pi's resolveAzureConfig: explicit base URL →
// AZURE_OPENAI_BASE_URL → AZURE_OPENAI_RESOURCE_NAME default host,
// normalized for Azure hosts.
func (p *AzureProvider) resolveBaseURL(opts llm.StreamOptions) (string, error) {
	base := opts.BaseURL
	if base == "" {
		base = p.BaseURL
	}
	if base == "" {
		base = os.Getenv("AZURE_OPENAI_BASE_URL")
	}
	if base == "" {
		if rn := os.Getenv("AZURE_OPENAI_RESOURCE_NAME"); rn != "" {
			base = "https://" + rn + ".openai.azure.com/openai/v1"
		}
	}
	if base == "" {
		return "", fmt.Errorf("azure-openai-responses: base URL is required (set AZURE_OPENAI_BASE_URL or AZURE_OPENAI_RESOURCE_NAME, or configure baseUrl)")
	}
	return normalizeAzureBaseURL(base)
}

// normalizeAzureBaseURL is pi's normalizeAzureBaseUrl: Azure hosts
// (openai.azure.com / cognitiveservices.azure.com / ai.azure.com) with a
// root-ish path are pinned to /openai/v1 with the query stripped; other
// (proxy) URLs pass through untouched, query included.
func normalizeAzureBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid Azure OpenAI base URL: %s", raw)
	}
	host := strings.ToLower(u.Hostname())
	isAzureHost := strings.HasSuffix(host, ".openai.azure.com") ||
		strings.HasSuffix(host, ".cognitiveservices.azure.com") ||
		strings.HasSuffix(host, ".ai.azure.com")
	path := strings.TrimRight(u.Path, "/")
	if isAzureHost && (path == "" || path == "/openai" || path == "/openai/v1/responses") {
		u.Path = "/openai/v1"
		u.RawQuery = ""
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func (p *AzureProvider) resolveAPIVersion() string {
	if p.APIVersion != "" {
		return p.APIVersion
	}
	if v := os.Getenv("AZURE_OPENAI_API_VERSION"); v != "" {
		return v
	}
	return DefaultAzureAPIVersion
}

func (p *AzureProvider) Stream(ctx context.Context, model llm.Model, t *llm.Transcript, opts llm.StreamOptions) (<-chan llm.Event, error) {
	key := p.resolveKey(opts)
	if key == "" {
		return nil, fmt.Errorf("azure-openai-responses: no API key (set AZURE_OPENAI_API_KEY or pass StreamOptions.APIKey)")
	}
	baseURL, err := p.resolveBaseURL(opts)
	if err != nil {
		return nil, err
	}
	// The Responses endpoint is NOT deployment-pathed (the OpenAI SDK's
	// AzureOpenAI only rewrites a fixed set of legacy endpoints); the
	// deployment travels in the body's model field and api-version in
	// the query.
	endpoint, err := url.Parse(baseURL + "/responses")
	if err != nil {
		return nil, err
	}
	q := endpoint.Query()
	q.Set("api-version", p.resolveAPIVersion())
	endpoint.RawQuery = q.Encode()

	req := buildAzureParams(model, t, opts)
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if os.Getenv("SCODE_DEBUG_REQ") == "1" {
		fmt.Fprintf(os.Stderr, "[scode] request: %s\n", body)
	}

	out := make(chan llm.Event, 64)
	go func() {
		defer close(out)
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, p.transport().Timeout())
			defer cancel()
		}
		headers := map[string][]string{
			"content-type": {"application/json"},
			"accept":       {"text/event-stream"},
			"api-key":      {key}, // Azure Responses authenticates via api-key, not Bearer
		}
		for k, v := range opts.ExtraHeaders {
			headers[strings.ToLower(k)] = []string{v}
		}
		resp, err := llm.PostStreamWithRetry(ctx, p.HTTP, endpoint.String(), headers, body, p.transport())
		if err != nil {
			p.emitError(out, model.ID, err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			p.emitError(out, model.ID, fmt.Sprintf("azure-openai-responses: HTTP %d: %s", resp.StatusCode, readErrorBody(resp.Body)))
			return
		}
		p.pump(ctx, resp.Body, model.ID, out)
	}()
	return out, nil
}

// buildAzureParams is pi's azure buildParams with default compat flags
// (strict mode ON → strict:false on tools; no prompt cache retention;
// no session-affinity headers).
func buildAzureParams(model llm.Model, t *llm.Transcript, opts llm.StreamOptions) *requestParams {
	msgs := t.Messages()

	params := &requestParams{
		Model:  resolveDeploymentName(model.ID),
		Input:  convertMessages(model, msgs, azureToolCallProviders),
		Stream: true,
		Store:  false,
		// Azure always routes the prompt cache by session (pi sends the
		// key unconditionally; there is no retention gate or TTL field).
		PromptCacheKey: clampPromptCacheKey(opts.PromptCacheKey),
	}

	maxTokens := opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = model.MaxTokens
	}
	if maxTokens > 0 && maxTokens < minOutputTokens {
		maxTokens = minOutputTokens
	}
	params.MaxOutputTokens = maxTokens

	if opts.Temperature != 0 {
		params.Temperature = &opts.Temperature
	}

	if tools := llm.CurrentTools(msgs); len(tools) > 0 {
		params.Tools = convertTools(tools, true) // supportsStrictMode defaults true on Azure
	}

	applyReasoning(params, model, opts.ThinkingLevel)
	return params
}

func (p *AzureProvider) pump(ctx context.Context, r io.Reader, model string, out chan<- llm.Event) {
	asm := newAssembler(model, p.Name())
	debug := os.Getenv("SCODE_DEBUG_SSE") == "1"
	// pi pushes start right after the request is accepted, before any
	// stream event.
	{
		msg := asm.snapshot()
		out <- llm.Event{Type: llm.EventStart, Message: &msg}
	}
	sawTerminal := false
	err := llm.ReadSSE(r, func(ev llm.SSEEvent) error {
		if debug {
			fmt.Fprintf(os.Stderr, "[scode] sse: %s\n", ev.Data)
		}
		events, terminal, err := asm.handle(ev.Data)
		for _, e := range events {
			select {
			case out <- e:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err != nil {
			return err
		}
		if terminal {
			sawTerminal = true
			return io.EOF
		}
		return nil
	})
	if err == nil && !sawTerminal {
		err = fmt.Errorf("stream ended before a terminal event")
	}
	if err != nil && err != io.EOF {
		msg := asm.snapshot()
		msg.StopReason = llm.StopError
		if ctx.Err() != nil {
			msg.StopReason = llm.StopAborted
			msg.Error = "aborted by caller"
		} else {
			msg.Error = err.Error()
		}
		select {
		case out <- llm.Event{Type: llm.EventError, Message: &msg, Reason: msg.StopReason, Err: err}:
		case <-ctx.Done():
		}
	}
}

func (p *AzureProvider) emitError(out chan<- llm.Event, model, text string) {
	msg := llm.Message{
		Role:       llm.RoleAssistant,
		Model:      model,
		Provider:   p.Name(),
		StopReason: llm.StopError,
		Error:      text,
	}
	out <- llm.Event{Type: llm.EventError, Message: &msg, Reason: llm.StopError, Err: fmt.Errorf("%s", text)}
}

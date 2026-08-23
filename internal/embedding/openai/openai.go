// Package openai adapts any OpenAI-compatible embeddings endpoint.
//
// As with the chat adapter, the base URL is configuration, so the same code serves OpenAI,
// Azure, and self-hosted servers. No type declared here escapes the package.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/embedding"
)

// Config configures the adapter. The API key is held in memory only.
type Config struct {
	BaseURL string
	APIKey  string
	Model   string
	// Dimensions requests a specific width from models that support truncation, which is
	// how a 3072-wide model is made to fit a 1536-wide projection.
	Dimensions int
	Timeout    time.Duration
	MaxRetries int
	HTTPClient *http.Client
}

// DefaultBaseURL is the public OpenAI endpoint.
const DefaultBaseURL = "https://api.openai.com/v1"

// Embedder is an OpenAI-compatible embeddings client.
type Embedder struct {
	cfg    Config
	client *http.Client
}

// New builds the adapter.
func New(cfg Config) (*Embedder, error) {
	const op = "embedding/openai.New"

	if cfg.Model == "" {
		return nil, domain.Errorf(domain.CodeInvalidArgument, op, "a model must be configured")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Dimensions <= 0 {
		cfg.Dimensions = embedding.Dimensions
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &Embedder{cfg: cfg, client: client}, nil
}

func (e *Embedder) Name() string    { return "openai" }
func (e *Embedder) Model() string   { return e.cfg.Model }
func (e *Embedder) Version() int    { return 1 }
func (e *Embedder) Dimensions() int { return e.cfg.Dimensions }

type embedRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Embed returns one vector per input, in input order.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([]embedding.Vector, error) {
	const op = "embedding/openai.Embed"

	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) > embedding.MaxBatch {
		return nil, domain.Errorf(domain.CodeInvalidArgument, op,
			"batch of %d exceeds the %d limit", len(texts), embedding.MaxBatch)
	}

	body, err := json.Marshal(embedRequest{
		Model:      e.cfg.Model,
		Input:      texts,
		Dimensions: e.cfg.Dimensions,
	})
	if err != nil {
		return nil, domain.Wrap(err, domain.CodeInternal, op, "cannot encode request")
	}

	var lastErr error
	for attempt := 0; attempt <= e.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, domain.Wrap(ctx.Err(), domain.CodeProviderUnavailable, op,
					"cancelled while retrying")
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}

		vectors, retryable, err := e.attempt(ctx, body, len(texts))
		if err == nil {
			return vectors, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}
	return nil, lastErr
}

func (e *Embedder) attempt(ctx context.Context, body []byte, want int) ([]embedding.Vector, bool, error) {
	const op = "embedding/openai.attempt"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.cfg.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, false, domain.Wrap(err, domain.CodeInternal, op, "cannot build request")
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, true, domain.Wrap(err, domain.CodeProviderUnavailable, op, "provider is unreachable")
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, true, domain.Wrap(err, domain.CodeProviderUnavailable, op,
			"cannot read provider response")
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, true, domain.Errorf(domain.CodeRateLimited, op, "provider rate limited the request")
	case resp.StatusCode >= 500:
		return nil, true, domain.Errorf(domain.CodeProviderUnavailable, op,
			"provider returned status %d", resp.StatusCode)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// Never echo the body: it can contain a fragment of the credential.
		return nil, false, domain.Errorf(domain.CodeProviderUnavailable, op,
			"provider rejected the credentials")
	case resp.StatusCode >= 400:
		return nil, false, domain.Errorf(domain.CodeInvalidArgument, op,
			"provider rejected the request with status %d", resp.StatusCode)
	}

	var parsed embedResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, false, domain.Wrap(err, domain.CodeProviderUnavailable, op,
			"provider returned a malformed response")
	}
	if parsed.Error != nil {
		return nil, false, domain.Errorf(domain.CodeProviderUnavailable, op,
			"provider reported an error: %s", parsed.Error.Type)
	}
	if len(parsed.Data) != want {
		return nil, false, domain.Errorf(domain.CodeProviderUnavailable, op,
			"provider returned %d embeddings for %d inputs", len(parsed.Data), want)
	}

	// Providers are permitted to return results out of order, and silently mismatching a
	// vector with its text would corrupt every search that touched it.
	sort.Slice(parsed.Data, func(i, j int) bool { return parsed.Data[i].Index < parsed.Data[j].Index })

	out := make([]embedding.Vector, 0, len(parsed.Data))
	for i, item := range parsed.Data {
		if item.Index != i {
			return nil, false, domain.Errorf(domain.CodeProviderUnavailable, op,
				"provider returned embeddings with gaps in their indices")
		}
		vector := embedding.Vector(item.Embedding)
		if err := vector.Validate(e.cfg.Dimensions); err != nil {
			return nil, false, err
		}
		out = append(out, vector.Normalize())
	}
	return out, false, nil
}

const maxResponseBytes = 32 << 20

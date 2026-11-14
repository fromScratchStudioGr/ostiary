package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/auth"
	"github.com/fromScratchStudioGr/ostiary/internal/cache"
	"github.com/fromScratchStudioGr/ostiary/internal/keystore"
	"github.com/fromScratchStudioGr/ostiary/internal/metrics"
	"github.com/fromScratchStudioGr/ostiary/internal/payload"
	"github.com/fromScratchStudioGr/ostiary/internal/providers"
	"github.com/fromScratchStudioGr/ostiary/internal/usage"

	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

// forceOpenAIStreamUsage rewrites an outbound OpenAI-schema streaming request
// so the provider always returns a usage object, regardless of what the
// client itself sent. Without this, a client gets billed nothing for a
// streaming request simply by leaving out stream_options (the default for
// most SDKs).
//
// This only ever strengthens a client's own request (turning requested-off
// or default-off usage reporting on); it never weakens or otherwise changes
// what the client asked for.
func forceOpenAIStreamUsage(pr *httputil.ProxyRequest, p payload.Payload) {
	var newBody []byte
	if p.Stream.Seen > 1 || p.StreamOptions.Seen > 1 {
		// A provider might read either copy of a repeated key, so the body is
		// re-encoded with exactly one of each: the ones read here.
		newBody = reencodeWithStreamUsage(p.Body)
	} else {
		newBody = patchWithStreamUsage(p)
	}
	if newBody == nil {
		return
	}

	pr.Out.Body = io.NopCloser(bytes.NewReader(newBody))
	pr.Out.ContentLength = int64(len(newBody))
	pr.Out.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
}

// patchWithStreamUsage returns the body with stream_options.include_usage
// set, splicing the new stream_options into the original bytes, or nil when
// the request isn't streaming.
func patchWithStreamUsage(p payload.Payload) []byte {
	body, stream, options := p.Body, p.Stream, p.StreamOptions

	var streaming bool
	if stream.Seen == 0 || json.Unmarshal(body[stream.Start:stream.End], &streaming) != nil || !streaming {
		return nil
	}

	var current []byte
	if options.Seen > 0 {
		current = body[options.Start:options.End]
	}
	newOptions := streamOptionsWithUsage(current)

	if options.Seen > 0 {
		newBody := make([]byte, 0, len(body)-len(current)+len(newOptions))
		newBody = append(newBody, body[:options.Start]...)
		newBody = append(newBody, newOptions...)
		return append(newBody, body[options.End:]...)
	}

	// The object has at least the stream member, so the new one follows a comma.
	closing := bytes.LastIndexByte(body, '}')
	const key = `,"stream_options":`
	newBody := make([]byte, 0, len(body)+len(key)+len(newOptions))
	newBody = append(newBody, body[:closing]...)
	newBody = append(newBody, key...)
	newBody = append(newBody, newOptions...)
	return append(newBody, body[closing:]...)
}

// reencodeWithStreamUsage re-encodes the top level of body so each key
// appears once (the last copy wins), with stream_options.include_usage set
// when the request streams. It re-encodes even when it doesn't: a provider
// reading the first of two stream keys could otherwise stream with no usage.
func reencodeWithStreamUsage(body []byte) []byte {
	var members map[string]json.RawMessage
	json.Unmarshal(body, &members)

	var streaming bool
	if json.Unmarshal(members["stream"], &streaming) == nil && streaming {
		members["stream_options"] = streamOptionsWithUsage(members["stream_options"])
	}

	newBody, _ := json.Marshal(members)
	return newBody
}

// streamOptionsWithUsage returns the client's stream_options with
// include_usage turned on, keeping its other fields. Anything that isn't an
// object is replaced.
func streamOptionsWithUsage(current []byte) []byte {
	var streamOptions map[string]json.RawMessage
	if json.Unmarshal(current, &streamOptions) != nil || streamOptions == nil {
		streamOptions = map[string]json.RawMessage{}
	}
	streamOptions["include_usage"] = json.RawMessage("true")
	newOptions, _ := json.Marshal(streamOptions)
	return newOptions
}

// logMissingUsage reports a successful answer that carries no usage, such as
// a stream an error event cut off before its usage, or a usage format the
// parser no longer reads: the provider bills it, and nothing is recorded.
// Error answers carry no usage and aren't billed, so they're left out.
func logMissingUsage(resp *http.Response, model providers.Model, usageInfo *usage.UsageInfo) {
	if usageInfo == nil && resp.StatusCode < 300 {
		slog.Error("UNBILLED USAGE: provider answer carries no usage, nothing billed", "model_id", model.Id, "provider", model.Provider, "status", resp.StatusCode)
	}
}

// chatCompletionHandler proxies a chat-completion request to the user's
// configured provider, decrypting their provider key just for this request
// and logging usage/cost afterward.
//
// drainLimit is how long a provider's answer is still read, for its usage,
// after the client that asked for it has gone away. usageMaxRetry is how many
// times a usage task is retried before asynq archives it.
func chatCompletionHandler(db *gorm.DB, redis *cache.Cache, kms keystore.KMSClient, asynqClient *asynq.Client, providerTransport *http.Transport, drainLimit time.Duration, usageMaxRetry int) http.HandlerFunc {
	return auth.RequireAPIKey(payload.Read(redis.HandleCache(func(w http.ResponseWriter, r *http.Request) {
		userCache := cache.UserCacheFromContext(r)
		p, _ := payload.FromContext(r)

		target, err := url.Parse(*userCache.ProviderURL + string(*userCache.ProviderEndpoint))
		if err != nil {
			slog.Error("chat-completion: invalid provider URL", "consumer_id", userCache.ConsumerID, "provider_key_id", userCache.ProviderKeyID, "error", err)
			api.WriteError(w, "Invalid provider URL", http.StatusInternalServerError)
			return
		}

		stopKMSDecrypt := metrics.Observe("kms_decrypt")
		plaintextKey, err := keystore.OpenProviderKey(
			r.Context(), kms,
			userCache.Ciphertext, userCache.Nonce,
			userCache.EncryptedDEK, userCache.KMSKeyID,
		)
		kmsDecryptDuration := stopKMSDecrypt()
		if err != nil {
			slog.Error("chat-completion: could not decrypt provider key", "consumer_id", userCache.ConsumerID, "provider_key_id", userCache.ProviderKeyID, "error", err)

			// The cached session may hold key material that no longer matches
			// the stored provider key; drop it so the next request reloads it.
			token, _ := auth.ExtractAPIKeyToken(r)
			if err := redis.InvalidateSession(r.Context(), token, userCache.ModelID); err != nil {
				slog.Error("chat-completion: could not invalidate cached session after decrypt failure", "consumer_id", userCache.ConsumerID, "model_id", userCache.ModelID, "error", err)
			}

			api.WriteError(w, "Could not decrypt provider key", http.StatusInternalServerError)
			return
		}
		defer keystore.Zero(plaintextKey)
		providerKey := string(plaintextKey)

		model, ok := providers.Models[userCache.ModelID]
		if !ok {
			slog.Warn("chat-completion: unsupported model", "consumer_id", userCache.ConsumerID, "model_id", userCache.ModelID)
			api.WriteError(w, "Unsupported model", http.StatusBadRequest)
			return
		}

		slog.Info("chat completion request", "consumer_id", userCache.ConsumerID)

		requestedAt := auth.RequestStartFromContext(r)

		// The provider request doesn't end when the client goes away: the
		// provider bills what it has generated, and the usage only arrives at
		// the end of its answer. It runs on, and is read to the end, for at
		// most drainLimit after the client has gone.
		upstreamCtx, cancelUpstream := context.WithCancel(context.WithoutCancel(r.Context()))
		stopWatching := context.AfterFunc(r.Context(), func() {
			time.AfterFunc(drainLimit, cancelUpstream)
		})
		upstreamDone := func() {
			stopWatching()
			cancelUpstream()
		}

		providerProxy := &httputil.ReverseProxy{
			Transport: providerTransport,
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.URL.Path = target.Path
				pr.Out.URL.RawPath = target.EscapedPath()
				// The client's Ostiary key may be in any of these. None of them
				// may reach the provider, whichever one the provider's key uses.
				for _, header := range []string{"Authorization", "x-api-key", "x-goog-api-key"} {
					pr.Out.Header.Del(header)
				}
				// With no Accept-Encoding of ours, Go's transport asks for gzip
				// and decompresses the answer itself, so the usage is always
				// read from plain bytes, whatever encodings the client accepts.
				pr.Out.Header.Del("Accept-Encoding")

				switch model.AuthenticationHeader {
				case providers.XAPIKey:
					pr.Out.Header.Set("x-api-key", providerKey)
				case providers.GoogAPIKey:
					pr.Out.Header.Set("x-goog-api-key", providerKey)
				case providers.Bearer:
					pr.Out.Header.Set("Authorization", "Bearer "+providerKey)
				default:
					slog.Error("chat-completion: unknown authentication header type, provider key not attached", "model_id", model.Id, "authentication_header", model.AuthenticationHeader)
				}

				if model.Endpoint == providers.OpenAIEndpoint {
					forceOpenAIStreamUsage(pr, p)
				}

				pr.Out = pr.Out.WithContext(upstreamCtx)
			},
			ModifyResponse: func(resp *http.Response) error {
				encoding := resp.Header.Get("Content-Encoding")

				var onClose func(data []byte)
				if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
					onClose = func(data []byte) {
						defer metrics.Observe("usage_parse_stream")()

						decoded, err := usage.DecodeContentEncoding(encoding, data)
						if err != nil {
							slog.Error("could not decode stream", "content_encoding", encoding, "error", err)
							return
						}

						usageInfo, err := usage.ParseStreamUsage(model, decoded)
						if err != nil {
							slog.Error("could not parse usage from stream, usage not logged", "model_id", model.Id, "provider", model.Provider, "error", err)
							return
						}
						logMissingUsage(resp, model, usageInfo)

						usage.LogUsage(r.Context(), asynqClient, db, redis, model, userCache.ConsumerID, userCache.ProviderKeyID, usageInfo, requestedAt, usageMaxRetry)
					}
				} else {
					onClose = func(data []byte) {
						defer metrics.Observe("usage_parse")()

						decoded, err := usage.DecodeContentEncoding(encoding, data)
						if err != nil {
							slog.Error("could not decode response", "content_encoding", encoding, "error", err)
							return
						}

						usageInfo, err := usage.ParseBodyUsage(model, decoded)
						if err != nil {
							slog.Error("could not parse usage from response, usage not logged", "model_id", model.Id, "provider", model.Provider, "error", err)
							return
						}
						logMissingUsage(resp, model, usageInfo)

						usage.LogUsage(r.Context(), asynqClient, db, redis, model, userCache.ConsumerID, userCache.ProviderKeyID, usageInfo, requestedAt, usageMaxRetry)
					}
				}

				resp.Body = &usage.UsageTeeBody{ReadCloser: resp.Body, OnClose: func(data []byte) {
					onClose(data)
					upstreamDone()
				}}
				return nil
			},
		}

		timeToForward := metrics.ObserveSince("time_to_forward", auth.RequestStartFromContext(r))
		metrics.ObserveDuration("time_to_forward_excl_kms", timeToForward-kmsDecryptDuration)
		slog.Info("chat-completion: phase durations",
			"consumer_id", userCache.ConsumerID,
			"kms_decrypt_ms", kmsDecryptDuration.Seconds()*1000,
			"time_to_forward_ms", timeToForward.Seconds()*1000,
		)

		stopRoundtrip := metrics.Observe("provider_roundtrip")
		providerProxy.ServeHTTP(w, r)
		stopRoundtrip()
	})))
}

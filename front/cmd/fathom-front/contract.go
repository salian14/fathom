package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spellbound/fathom/fathom-front/internal/config"
)

// contractSchemaVersion is the version of the document this file emits, not of
// anything it describes. Bump it when the shape below changes so a reader that
// caches contracts can tell a reshape from a real interface change.
const contractSchemaVersion = 1

// wantContract reports whether the process was asked to print its contract
// instead of serving. Hand-rolled rather than the flag package for the reason
// given in fathom-mcp's contract.go: config here is environment-only, and this
// binary should not grow a flag surface.
func wantContract(args []string) bool {
	for _, a := range args {
		if a == "-contract" || a == "--contract" {
			return true
		}
	}
	return false
}

// contract prints this service's published interface as JSON on stdout.
//
// The paths come from routes(), the same slice newMux walks, so the spec
// cannot describe a route that is not served or miss one that is. What routes()
// cannot supply is body shapes -- Go has no OpenAPI generator and the two
// interesting bodies here belong to other services anyway -- so the proxied
// routes are published as pass-throughs naming their upstream contract, and
// only the bodies this process actually writes are given schemas.
func contract(w io.Writer) error {
	restore := clearEnvPrefix("FATHOM_FRONT_")
	defer restore()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading default config: %w", err)
	}

	rs := routes()
	summary := make([]map[string]any, 0, len(rs))
	for _, r := range rs {
		entry := map[string]any{
			"pattern": r.Pattern,
			"methods": r.Methods,
			"summary": r.Summary,
			"subtree": strings.HasSuffix(r.Pattern, "/"),
		}
		if r.Upstream != "" {
			entry["upstream"] = r.Upstream
		}
		if r.GatedOn != "" {
			entry["gated_on"] = r.GatedOn
		}
		summary = append(summary, entry)
	}

	doc := map[string]any{
		"schema_version": contractSchemaVersion,
		"component":      "front",
		"service":        "fathom-front",
		"routes":         summary,
		"probes": map[string]string{
			"liveness":  "/healthz",
			"readiness": "/readyz",
		},
		// Recorded by the config helpers during the Load above, so it is what
		// Load reads rather than a table kept beside it.
		"config": config.Describe(),
		"telemetry": map[string]any{
			"traces":  true,
			"metrics": true,
			// This service defines no instruments of its own: its metrics come
			// from otelhttp on the two boundaries, whose names are the HTTP
			// semantic conventions rather than anything published here.
			"instruments":  []any{},
			"exporter_var": "FATHOM_FRONT_TELEMETRY",
			"note": "The one service that is a trace root: a browser sends no " +
				"traceparent, so unlike fathom-mcp this does not drop untraced " +
				"inbound requests. Only /healthz and /readyz are excluded.",
		},
		"defaults": map[string]any{
			"addr":           cfg.Addr,
			"agent_url":      cfg.AgentURL,
			"agent_timeout":  cfg.AgentTimeout.String(),
			"eval_timeout":   cfg.EvalTimeout.String(),
			"ready_timeout":  cfg.ReadyTimeout.String(),
			"max_body_bytes": cfg.MaxBodyBytes,
		},
		"openapi": openapiDoc(rs),
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// openapiDoc renders routes() as an OpenAPI 3.1 document.
func openapiDoc(rs []route) map[string]any {
	paths := map[string]any{}
	for _, r := range rs {
		pattern, params := openapiPath(r)
		op := map[string]any{
			"summary":   r.Summary,
			"responses": responsesFor(r),
		}
		if len(params) > 0 {
			op["parameters"] = params
		}
		if r.GatedOn != "" {
			op["description"] = "Registered only when " + r.GatedOn + " is set. " +
				"With no URL there is no client, no route and no advertised capability; " +
				"a browser learns which gates are open from GET /api/features."
		}
		if r.Upstream != "" {
			op["x-proxies-to"] = r.Upstream
		}
		if r.Pattern == "/api/chat" {
			op["requestBody"] = map[string]any{
				"required": true,
				"content": map[string]any{
					"application/json": map[string]any{
						"schema": map[string]any{
							"$ref": "https://fathom.invalid/contracts/agent/openapi.json#/components/schemas/ChatRequest",
						},
					},
				},
			}
		}

		item := map[string]any{}
		for _, m := range r.Methods {
			item[strings.ToLower(m)] = op
		}
		paths[pattern] = item
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "fathom-front",
			"version": version,
			"description": "The browser-facing surface of Fathom. Holds no Kubernetes " +
				"credentials and no conversation state: the transcript lives in the page, " +
				"because the agent's /chat is stateless by design.",
		},
		"paths": paths,
		"components": map[string]any{
			"schemas": map[string]any{
				"Error": map[string]any{
					"type":        "object",
					"required":    []string{"error"},
					"description": "The same envelope fathom-agent uses, so the page has one shape to parse rather than one per failure origin.",
					"properties": map[string]any{
						"error": map[string]any{"type": "string"},
					},
				},
				"Features": map[string]any{
					"type":        "object",
					"required":    []string{"eval"},
					"description": "Which server-side gates are open.",
					"properties": map[string]any{
						"eval": map[string]any{
							"type":        "boolean",
							"description": "Whether the eval view has a backend to read.",
						},
					},
				},
			},
		},
		// Not an OpenAPI field. The status mapping lives in proxy.classify and
		// is the thing a caller actually has to handle; 499 in particular is
		// not a registered code and will not be guessed.
		"x-proxy-status-mapping": map[string]string{
			"400": "path outside the route prefix, or containing ..",
			"405": "method not accepted by this route; the Allow header lists what is",
			"413": "request body over FATHOM_FRONT_MAX_BODY_BYTES",
			"499": "the browser aborted (Stop, navigation, reload). Non-standard, and deliberate: nothing is wrong and nobody is listening",
			"502": "upstream unreachable",
			"504": "upstream did not answer within this route's timeout",
		},
	}
}

// openapiPath turns a ServeMux pattern into an OpenAPI path. The one subtree
// route becomes a templated parameter, which is the closest OpenAPI gets to
// "everything under here".
func openapiPath(r route) (string, []map[string]any) {
	if !strings.HasSuffix(r.Pattern, "/") || r.Pattern == "/" {
		return r.Pattern, nil
	}
	return r.Pattern + "{subpath}", []map[string]any{{
		"name":        "subpath",
		"in":          "path",
		"required":    true,
		"description": "Forwarded to the upstream with the " + strings.TrimSuffix(r.Pattern, "/") + " prefix stripped.",
		"schema":      map[string]any{"type": "string"},
	}}
}

func responsesFor(r route) map[string]any {
	plain := func(desc string) map[string]any {
		return map[string]any{
			"description": desc,
			"content": map[string]any{
				"text/plain": map[string]any{"schema": map[string]any{"type": "string"}},
			},
		}
	}
	jsonRef := func(desc, ref string) map[string]any {
		return map[string]any{
			"description": desc,
			"content": map[string]any{
				"application/json": map[string]any{"schema": map[string]any{"$ref": ref}},
			},
		}
	}
	errRef := func(desc string) map[string]any {
		return jsonRef(desc, "#/components/schemas/Error")
	}

	switch r.Pattern {
	case "/healthz":
		return map[string]any{"200": plain("ok")}
	case "/readyz":
		return map[string]any{
			"200": plain("The agent reported ready."),
			"503": plain("The agent's own reason, forwarded verbatim so the failing dependency is named."),
		}
	case "/api/features":
		return map[string]any{
			"200": jsonRef("Cache-Control: no-store.", "#/components/schemas/Features"),
		}
	case "/api/chat":
		return map[string]any{
			"200": jsonRef("The agent's answer, passed through unchanged.",
				"https://fathom.invalid/contracts/agent/openapi.json#/components/schemas/ChatResponse"),
			"405": errRef("Only POST reaches this route."),
			"413": errRef("Body over the configured limit."),
			"499": errRef("The browser aborted the request."),
			"502": errRef("The agent was unreachable."),
			"504": errRef("The agent did not answer within FATHOM_FRONT_AGENT_TIMEOUT."),
		}
	case "/api/eval/":
		return map[string]any{
			"200": map[string]any{
				"description": "The eval read service's response, passed through unchanged. See contracts/eval/openapi.json.",
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"type": "object"}},
				},
			},
			"400": errRef("Path outside /api/eval, or containing '..'."),
			"405": errRef("Only GET reaches this route."),
			"499": errRef("The browser aborted the request."),
			"502": errRef("The eval service was unreachable."),
			"504": errRef("The eval service did not answer within FATHOM_FRONT_EVAL_TIMEOUT."),
		}
	default:
		return map[string]any{
			"200": map[string]any{"description": "A page or asset embedded in the binary."},
			"404": map[string]any{"description": "No such asset. An unconfigured /api/eval/* lands here too."},
		}
	}
}

// clearEnvPrefix unsets every variable with the given prefix and returns a
// function that puts them back, so a dump reports the shipped defaults rather
// than the operator's shell.
func clearEnvPrefix(prefix string) func() {
	saved := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(k, prefix) {
			saved[k] = v
			os.Unsetenv(k)
		}
	}
	return func() {
		for k, v := range saved {
			os.Setenv(k, v)
		}
	}
}

package api

import (
	"net/http"

	"github.com/example/aegisroute/internal/httperror"
)

// modelObject is one entry in the OpenAI-style /v1/models list. owned_by is a
// fixed marker so clients built against the OpenAI schema parse the response
// unchanged.
type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// modelList is the OpenAI-style envelope wrapping the served models.
type modelList struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

// listModels returns the logical models served by enabled backends, in the
// OpenAI /v1/models shape, de-duplicated by model name (two backends serving
// "llama-fast" appear once). Order follows the store's (priority, name)
// ordering, so the list is stable across calls.
func listModels(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		backends, err := deps.Backends.ListEnabled(r.Context())
		if err != nil {
			httperror.Write(w, r, http.StatusInternalServerError,
				httperror.CodeInternal, "could not list models")
			return
		}

		seen := make(map[string]struct{}, len(backends))
		data := make([]modelObject, 0, len(backends))
		for _, b := range backends {
			if _, dup := seen[b.ModelName]; dup {
				continue
			}
			seen[b.ModelName] = struct{}{}
			data = append(data, modelObject{
				ID:      b.ModelName,
				Object:  "model",
				OwnedBy: "aegisroute",
			})
		}

		writeJSON(w, http.StatusOK, modelList{Object: "list", Data: data})
	}
}

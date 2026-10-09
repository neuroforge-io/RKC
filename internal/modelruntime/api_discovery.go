package modelruntime

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ModelCatalog is one bounded metadata page. It is not a schema-support,
// inference-access, deployment-quality, price, or account-entitlement check.
type ModelCatalog struct {
	Models        []CatalogModel `json:"models"`
	MoreAvailable bool           `json:"more_available"`
}

// CatalogModel identifies a provider-reported model option.
type CatalogModel struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name,omitempty"`
	SupportedGenerations []string `json:"supported_generation_methods,omitempty"`
}

// DiscoverModels explicitly performs one metadata GET against the selected
// endpoint origin and API prefix. It never generates text or follows pagination,
// redirects or proxies; callers surface MoreAvailable when a page is incomplete.
func (provider *APIProvider) DiscoverModels(ctx context.Context) (ModelCatalog, error) {
	if provider == nil || ctx == nil {
		return ModelCatalog{}, errors.New("API provider and context are required")
	}
	if err := ctx.Err(); err != nil {
		return ModelCatalog{}, err
	}
	provider.mutex.Lock()
	if provider.closed || provider.cancel != nil {
		provider.mutex.Unlock()
		return ModelCatalog{}, errors.New("API provider is closed or busy")
	}
	requestContext, cancel := context.WithCancel(ctx)
	provider.cancel = cancel
	provider.mutex.Unlock()
	defer func() { cancel(); provider.mutex.Lock(); provider.cancel = nil; provider.mutex.Unlock() }()
	endpoint := provider.profile.Endpoint
	switch provider.profile.Provider {
	case "anthropic":
		endpoint = strings.TrimSuffix(endpoint, "/messages") + "/models"
	case "gemini":
		endpoint = strings.TrimSuffix(endpoint, "/models/"+provider.profile.Model+":generateContent") + "/models"
	default:
		endpoint = strings.TrimSuffix(endpoint, "/chat/completions") + "/models"
	}
	output, err := provider.requestJSON(requestContext, http.MethodGet, endpoint, nil)
	if err != nil {
		return ModelCatalog{}, err
	}
	catalog := ModelCatalog{Models: []CatalogModel{}}
	if provider.profile.Provider == "gemini" {
		var envelope struct {
			Models []struct {
				Name    string   `json:"name"`
				Display string   `json:"displayName"`
				Methods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
			NextPageToken string `json:"nextPageToken"`
		}
		if decodeSingleJSONObject(output, &envelope, false) != nil || envelope.Models == nil || len(envelope.Models) > 1000 {
			return ModelCatalog{}, invalidAPIOutput("invalid model catalog")
		}
		catalog.MoreAvailable = envelope.NextPageToken != ""
		for _, model := range envelope.Models {
			id := strings.TrimPrefix(model.Name, "models/")
			if !validModelID(id) || len(model.Methods) > 32 || !validCatalogDisplay(model.Display) {
				return ModelCatalog{}, invalidAPIOutput("invalid model catalog entry")
			}
			for _, method := range model.Methods {
				if !validModelID(method) {
					return ModelCatalog{}, invalidAPIOutput("invalid catalog generation method")
				}
			}
			catalog.Models = append(catalog.Models, CatalogModel{ID: id, Name: model.Display, SupportedGenerations: model.Methods})
		}
	} else {
		var envelope struct {
			Data []struct {
				ID   string `json:"id"`
				Name string `json:"display_name"`
			} `json:"data"`
			HasMore bool `json:"has_more"`
		}
		if decodeSingleJSONObject(output, &envelope, false) != nil || envelope.Data == nil || len(envelope.Data) > 1000 {
			return ModelCatalog{}, invalidAPIOutput("invalid model catalog")
		}
		catalog.MoreAvailable = envelope.HasMore
		for _, model := range envelope.Data {
			if !validModelID(model.ID) || !validCatalogDisplay(model.Name) {
				return ModelCatalog{}, invalidAPIOutput("invalid model catalog entry")
			}
			catalog.Models = append(catalog.Models, CatalogModel{ID: model.ID, Name: model.Name})
		}
	}
	return catalog, nil
}

func validCatalogDisplay(value string) bool {
	return len(value) <= 1024 && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

// Assert the adapter supplies the same optional capabilities and prompt
// contracts used to bind grounded-answer provenance to transmitted requests.
var _ Provider = (*APIProvider)(nil)
var _ CapabilityProvider = (*APIProvider)(nil)
var _ PromptBuilder = (*APIProvider)(nil)

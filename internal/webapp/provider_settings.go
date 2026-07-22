package webapp

import (
	"net/http"

	"github.com/rexzhao/simple-agent/internal/execution"
)

func (s *Server) handleProviderSettings(w http.ResponseWriter, r *http.Request) {
	document, err := s.service.ProviderSettings(r.PathValue("projectID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	for index := range document.Providers {
		if document.Providers[index].CodexAuth == nil {
			continue
		}
		status, err := s.codexLogins.status(document.ProjectID, document.Providers[index].Name)
		if err == nil {
			document.Providers[index].CodexAuth = &status
		}
	}
	writeJSON(w, http.StatusOK, document)
}

func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var input execution.ProviderSettingsInput
	if !decodeJSON(w, r, &input) {
		return
	}
	document, err := s.service.CreateProviderSettings(r.PathValue("projectID"), input)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, document)
}

func (s *Server) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	var input execution.ProviderSettingsInput
	if !decodeJSON(w, r, &input) {
		return
	}
	document, err := s.service.UpdateProviderSettings(r.PathValue("projectID"), r.PathValue("providerName"), input)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, document)
}

func (s *Server) handleUpdateDefaultProviderModel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	document, err := s.service.UpdateDefaultProviderModel(r.PathValue("projectID"), input.Provider, input.Model)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, document)
}

func (s *Server) handleDiscoverProviderModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.service.DiscoverProviderModels(r.Context(), r.PathValue("projectID"), r.PathValue("providerName"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (s *Server) handleStartCodexLogin(w http.ResponseWriter, r *http.Request) {
	status, err := s.codexLogins.start(r.PathValue("projectID"), r.PathValue("providerName"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, status)
}

func (s *Server) handleCodexLoginStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.codexLogins.status(r.PathValue("projectID"), r.PathValue("providerName"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleClearCodexLogin(w http.ResponseWriter, r *http.Request) {
	if err := s.codexLogins.clear(r.PathValue("projectID"), r.PathValue("providerName")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

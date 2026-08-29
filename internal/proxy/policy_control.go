package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"unicode/utf8"
)

const (
	RuntimePolicyPath              = "/_cq/control/policy"
	RuntimePolicyPoolPath          = "/_cq/control/policy/pool"
	RuntimePolicySessionDigestPath = "/_cq/control/policy/session-digest"
	canonicalSessionIDMaxBytes     = 4096
)

type PoolMutationRequest struct {
	Operation string    `json:"operation"`
	Name      string    `json:"name"`
	NewName   string    `json:"new_name,omitempty"`
	Value     PoolValue `json:"value,omitempty"`
}

func (s *Server) handlePolicyControl(writer http.ResponseWriter, request *http.Request) {
	if s == nil || s.RoutingPolicy == nil || s.SessionPolicy == nil {
		http.Error(writer, "routing policy unavailable", http.StatusServiceUnavailable)
		return
	}
	switch request.Method {
	case http.MethodGet:
		document, err := s.RoutingPolicy.Document()
		if err != nil {
			http.Error(writer, "routing policy unavailable", http.StatusServiceUnavailable)
			return
		}
		writePolicyControlJSON(writer, document)
	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(request.Body, routingPolicyMaxBytes+1))
		if err != nil || len(body) > routingPolicyMaxBytes {
			http.Error(writer, "invalid routing policy", http.StatusBadRequest)
			return
		}
		var policy RoutingPolicyDocument
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&policy); err != nil {
			http.Error(writer, "invalid routing policy", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			http.Error(writer, "invalid routing policy", http.StatusBadRequest)
			return
		}
		if err := s.RoutingPolicy.PublishDocument(policy); err != nil {
			http.Error(writer, "routing policy rejected", http.StatusConflict)
			return
		}
		current := s.RoutingPolicy.Current()
		s.SessionPolicy.Replace(current)
		document, err := s.RoutingPolicy.Document()
		if err != nil {
			http.Error(writer, "routing policy unavailable", http.StatusServiceUnavailable)
			return
		}
		writePolicyControlJSON(writer, document)
	default:
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handlePolicyPoolControl(writer http.ResponseWriter, request *http.Request) {
	if s == nil || s.RoutingPolicy == nil || s.SessionPolicy == nil {
		http.Error(writer, "routing policy unavailable", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, routingPolicyMaxBytes+1))
	if err != nil || len(body) > routingPolicyMaxBytes {
		http.Error(writer, "invalid pool mutation", http.StatusBadRequest)
		return
	}
	var mutation PoolMutationRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&mutation); err != nil {
		http.Error(writer, "invalid pool mutation", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(writer, "invalid pool mutation", http.StatusBadRequest)
		return
	}
	switch mutation.Operation {
	case "rename":
		err = s.RoutingPolicy.RenamePool(mutation.Name, mutation.NewName)
	case "value":
		if mutation.NewName != "" {
			err = errors.New("invalid pool mutation")
		} else {
			err = s.RoutingPolicy.SetPoolValue(mutation.Name, mutation.Value)
		}
	default:
		err = errors.New("invalid pool mutation")
	}
	if err != nil {
		writePoolMutationError(writer, err)
		return
	}
	s.SessionPolicy.Replace(s.RoutingPolicy.Current())
	document, err := s.RoutingPolicy.Document()
	if err != nil {
		http.Error(writer, "routing policy unavailable", http.StatusServiceUnavailable)
		return
	}
	writePolicyControlJSON(writer, document)
}

func writePoolMutationError(writer http.ResponseWriter, err error) {
	code := "pool_mutation_rejected"
	switch {
	case errors.Is(err, ErrPoolNameInvalid):
		code = "invalid_pool_name"
	case errors.Is(err, ErrPoolNotFound):
		code = "pool_not_found"
	case errors.Is(err, ErrPoolNameConflict):
		code = "pool_name_conflict"
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(writer).Encode(struct {
		Error string `json:"error"`
	}{Error: code})
}

func (s *Server) handlePolicySessionDigest(writer http.ResponseWriter, request *http.Request) {
	if s == nil || s.RoutingPolicy == nil {
		http.Error(writer, "routing policy unavailable", http.StatusServiceUnavailable)
		return
	}
	session, err := io.ReadAll(io.LimitReader(request.Body, canonicalSessionIDMaxBytes+1))
	if err != nil || !validCanonicalSessionID(session) {
		http.Error(writer, "invalid session ID", http.StatusBadRequest)
		return
	}
	defer zeroRuntimeBytes(session)
	writePolicyControlJSON(writer, struct {
		SessionDigest string `json:"session_digest"`
	}{SessionDigest: s.RoutingPolicy.SessionDigest(session)})
}

func validCanonicalSessionID(session []byte) bool {
	if len(session) == 0 || len(session) > canonicalSessionIDMaxBytes || !utf8.Valid(session) {
		return false
	}
	for _, value := range session {
		if value < 0x20 || value == 0x7f {
			return false
		}
	}
	return true
}

func writePolicyControlJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		http.Error(writer, "routing policy response unavailable", http.StatusInternalServerError)
	}
}

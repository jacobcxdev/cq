package proxy

import (
	"context"
	"net/http"
	"time"
)

type runtimeCallerContextKey struct{}
type runtimeCallerIdentityContextKey struct{}

func withRuntimeCallerAuthority(ctx context.Context, caller RuntimeCallerAuthorityV1) context.Context {
	return context.WithValue(ctx, runtimeCallerContextKey{}, caller)
}

func runtimeCallerAuthority(ctx context.Context) (RuntimeCallerAuthorityV1, bool) {
	caller, ok := ctx.Value(runtimeCallerContextKey{}).(RuntimeCallerAuthorityV1)
	return caller, ok
}

func withRuntimeCallerIdentity(ctx context.Context, identity string) context.Context {
	return context.WithValue(ctx, runtimeCallerIdentityContextKey{}, identity)
}

func runtimeCallerIdentity(ctx context.Context) (string, bool) {
	identity, ok := ctx.Value(runtimeCallerIdentityContextKey{}).(string)
	return identity, ok && identity != ""
}

func validRuntimeCallerAuthority(caller RuntimeCallerAuthorityV1, method, requestURI string, now time.Time) bool {
	return caller.SchemaVersion == 1 && caller.Kind == "provider_branch_admission_consumed_v1" &&
		validNormalCallerDomain(caller.Domain) && caller.SubjectID != "" && caller.BearerFingerprint != "" &&
		caller.IndexEpoch != 0 && len(caller.AdmissionID) == 32 && len(caller.SingleUseNonce) == 32 &&
		len(caller.RequestNonce) == 32 && caller.Method == method && caller.RequestURI == requestURI &&
		caller.ConsumptionDigest != "" && caller.MAC != "" && now.Before(caller.ValidUntil)
}

func normalWorkerHandler(handler http.Handler, credentials []NormalCallerCredentialV1) http.Handler {
	return normalWorkerHandlerWithSource(handler, func(context.Context) ([]NormalCallerCredentialV1, error) {
		return credentials, nil
	})
}

func normalWorkerHandlerWithSource(handler http.Handler, credentials func(context.Context) ([]NormalCallerCredentialV1, error)) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		policy := normalCallerPolicy(request)
		if policy == normalCallerRoutePublic {
			handler.ServeHTTP(writer, request)
			return
		}
		caller, ok := runtimeCallerAuthority(request.Context())
		if !ok || !validRuntimeCallerAuthority(caller, request.Method, request.URL.RequestURI(), time.Now()) || !policyAllowsCaller(policy, caller.Domain) {
			http.Error(writer, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		current, err := credentials(request.Context())
		if err != nil {
			http.Error(writer, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		var bearer string
		var identity string
		for _, credential := range current {
			if credential.Domain == caller.Domain && credential.SubjectID == caller.SubjectID {
				if bearer != "" && (bearer != credential.Bearer || identity != credential.identity) {
					http.Error(writer, http.StatusText(http.StatusForbidden), http.StatusForbidden)
					return
				}
				bearer = credential.Bearer
				identity = credential.identity
			}
		}
		if bearer == "" {
			http.Error(writer, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		request.Header.Set("Authorization", "Bearer "+bearer)
		if identity != "" {
			request = request.WithContext(withRuntimeCallerIdentity(request.Context(), identity))
		}
		handler.ServeHTTP(writer, request)
		request.Header.Del("Authorization")
	})
}

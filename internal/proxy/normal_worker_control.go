package proxy

import (
	"context"
	"net/http"
	"time"
)

type runtimeCallerContextKey struct{}

func withRuntimeCallerAuthority(ctx context.Context, caller RuntimeCallerAuthorityV1) context.Context {
	return context.WithValue(ctx, runtimeCallerContextKey{}, caller)
}

func runtimeCallerAuthority(ctx context.Context) (RuntimeCallerAuthorityV1, bool) {
	caller, ok := ctx.Value(runtimeCallerContextKey{}).(RuntimeCallerAuthorityV1)
	return caller, ok
}

func validRuntimeCallerAuthority(caller RuntimeCallerAuthorityV1, method, requestURI string, now time.Time) bool {
	return caller.SchemaVersion == 1 && caller.Kind == "provider_branch_admission_consumed_v1" &&
		validNormalCallerDomain(caller.Domain) && caller.SubjectID != "" && caller.BearerFingerprint != "" &&
		caller.IndexEpoch != 0 && len(caller.AdmissionID) == 32 && len(caller.SingleUseNonce) == 32 &&
		len(caller.RequestNonce) == 32 && caller.Method == method && caller.RequestURI == requestURI &&
		caller.ConsumptionDigest != "" && caller.MAC != "" && now.Before(caller.ValidUntil)
}

func normalWorkerHandler(handler http.Handler, credentials []NormalCallerCredentialV1) http.Handler {
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
		var bearer string
		for _, credential := range credentials {
			if credential.Domain == caller.Domain && credential.SubjectID == caller.SubjectID {
				if bearer != "" && bearer != credential.Bearer {
					http.Error(writer, http.StatusText(http.StatusForbidden), http.StatusForbidden)
					return
				}
				bearer = credential.Bearer
			}
		}
		if bearer == "" {
			http.Error(writer, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		request.Header.Set("Authorization", "Bearer "+bearer)
		handler.ServeHTTP(writer, request)
		request.Header.Del("Authorization")
	})
}

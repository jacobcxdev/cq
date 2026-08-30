package proxy

import (
	"runtime"
	"testing"
)

func TestCandidateConfinementRejectsCredentialAndProviderAuthority(t *testing.T) {
	base := CandidateLaunchSpec{CandidateRoot: "/candidate", Inherited: []CandidateInheritedDescriptor{{FD: 3, Purpose: CandidateControllerIPC}}}
	tests := map[string]func(*CandidateLaunchSpec){
		"bearer": func(s *CandidateLaunchSpec) { s.ProviderBearer = []byte("secret") },
		"provider socket": func(s *CandidateLaunchSpec) {
			s.Inherited = append(s.Inherited, CandidateInheritedDescriptor{FD: 4, Purpose: "provider_socket"})
		},
		"origin":         func(s *CandidateLaunchSpec) { s.ProviderOrigin = "https://provider.invalid" },
		"authority key":  func(s *CandidateLaunchSpec) { s.AuthorityKey = []byte("authority") },
		"direct network": func(s *CandidateLaunchSpec) { s.DirectNetwork = true },
		"external file":  func(s *CandidateLaunchSpec) { s.ExternalPaths = []string{"/Users/example/.config"} },
		"executable":     func(s *CandidateLaunchSpec) { s.Executable = "/bin/sh" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			spec := base
			spec.Inherited = append([]CandidateInheritedDescriptor(nil), base.Inherited...)
			mutate(&spec)
			if err := ValidateCandidateConfinement(spec); err == nil {
				t.Fatal("accepted forbidden candidate authority")
			}
		})
	}
	err := ValidateCandidateConfinement(base)
	if runtime.GOOS == "darwin" {
		if err != nil {
			t.Fatalf("safe candidate rejected: %v", err)
		}
	} else if runtime.GOOS == "linux" {
		if err != nil && err.Error() != "candidate platform confinement unavailable" {
			t.Fatalf("Linux candidate error = %v", err)
		}
	} else if err == nil || err.Error() != "candidate platform confinement unavailable" {
		t.Fatalf("unsupported platform error = %v", err)
	}
}

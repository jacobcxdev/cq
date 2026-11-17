package history

import "math"

// ReadEstimates returns persisted burn evidence without advancing observations
// or writing history. Missing history retains the store's cold-start semantics.
func (s *Store) ReadEstimates() (RateEstimates, error) {
	state, err := s.load()
	if err != nil {
		return nil, err
	}
	estimates := make(RateEstimates)
	for _, account := range state.Accounts {
		if account == nil || account.ProviderID == "" || account.AccountKey == "" {
			continue
		}
		for window, value := range account.Windows {
			if value == nil || value.Samples < 2 || math.IsNaN(value.EWMARatePctPerS) || math.IsInf(value.EWMARatePctPerS, 0) || value.EWMARatePctPerS < 0 {
				continue
			}
			key := BurnRateKey{ProviderID: account.ProviderID, AccountKey: account.AccountKey, Window: window}
			estimates[key] = RateEstimate{RatePctPerS: value.EWMARatePctPerS, Samples: value.Samples, LastSeenUnix: value.LastSeenUnix}
		}
	}
	return estimates, nil
}

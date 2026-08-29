package userdirs

type Roots struct {
	Config  string
	State   string
	Cache   string
	Runtime string
	Logs    string
}

type Resolver struct {
	Getenv         func(string) string
	UserHomeDir    func() (string, error)
	UserCacheDir   func() (string, error)
	TempDir        func() string
	RoamingAppData func() (string, error)
	LocalAppData   func() (string, error)
}

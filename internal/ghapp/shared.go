package ghapp

// The shared xu526 GitHub App. Its client ID is public: with GitHub's
// device flow, signing in and refreshing need nothing else, so no app
// secret or private key is ever on a user's computer. Each computer keeps
// only its own user's refresh token, sealed with a local key.
//
// Set these to the shared app's values (or at build time:
// -ldflags "-X xu526/internal/ghapp.SharedClientID=Iv23… -X xu526/internal/ghapp.SharedAppSlug=xu526").
// XU_GITHUB_CLIENT_ID and XU_GITHUB_APP_SLUG override them; with an
// XU_GITHUB_APP_ID, xu526 uses its own registered app instead (full mode).
var (
	SharedClientID = ""
	SharedAppSlug  = "xu526"
)

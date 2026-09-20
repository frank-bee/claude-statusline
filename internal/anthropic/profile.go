package anthropic

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// What the plan is, and why the tool has to ask rather than assume.
//
// GET /api/oauth/profile says which kind of subscription the current token
// belongs to. This matters because the two things worth putting in a status
// line are metered differently and are mutually exclusive:
//
//	Pro, Max, Team   rate-limit windows (a 5-hour block, a weekly pool)
//	usage-based      a credit pool in money, and no windows at all
//
// On an Enterprise usage-based seat every window in /oauth/usage comes back
// null and Claude Code sends no rate_limits in the status line payload, so a
// windows-only status line renders nothing and looks broken. On Pro the credit
// pool reports spend.enabled false and the budget renders nothing instead.
// Knowing the plan is what lets one configuration be right on both.

// ProfileTTL is how long a cached profile is served. The plan changes when a
// seat is reassigned, which is rare - unlike usage, which moves constantly.
//
// The TTL is only half of what keeps the cache honest. The cache path is
// derived from the config directory, not from the account, and the credentials
// in a directory can be replaced by something that answers for a different
// account; a purely time-based cache would then serve a confidently wrong plan
// for the rest of the day. So the fingerprint of the credentials a profile was
// fetched with is recorded beside it, and a cache that does not match the
// credentials in force is stale no matter how fresh it is.
const ProfileTTL = 24 * time.Hour

// Plan is the kind of subscription the current credentials belong to.
type Plan string

// The plans this distinguishes. Unknown covers a response whose shape is new
// to us; callers treat it as "show what the payload offers and nothing more".
const (
	PlanUnknown    Plan = "unknown"
	PlanFree       Plan = "free"
	PlanPro        Plan = "pro"
	PlanMax        Plan = "max"
	PlanTeam       Plan = "team"
	PlanEnterprise Plan = "enterprise"
)

// MetersBudget reports whether this plan meters a credit pool in money, which
// is what the credits module renders.
func (p Plan) MetersBudget() bool { return p == PlanEnterprise }

// MetersWindows reports whether this plan meters rate-limit windows, which is
// what the usage module renders from the status line payload.
func (p Plan) MetersWindows() bool {
	return p == PlanPro || p == PlanMax || p == PlanTeam || p == PlanFree
}

// Profile is the part of the profile response this tool reads.
type Profile struct {
	Account struct {
		Email        string `json:"email"`
		HasClaudePro bool   `json:"has_claude_pro"`
		HasClaudeMax bool   `json:"has_claude_max"`
	} `json:"account"`
	Organization struct {
		Name string `json:"name"`
		// Type is claude_enterprise, claude_team and so on.
		Type string `json:"organization_type"`
		// SeatTier distinguishes a usage-based seat from a fixed one within
		// the same organization type, which is what decides budget metering.
		SeatTier string `json:"seat_tier"`
	} `json:"organization"`

	// Age is how long ago the cache this came from was written. Not part of
	// the API response.
	Age time.Duration `json:"-"`
}

// Plan classifies the profile. Organization type is checked before the
// personal has_claude_* flags, because an Enterprise seat reports both of
// those false while still being the plan in force.
func (p Profile) Plan() Plan {
	switch p.Organization.Type {
	case "claude_enterprise":
		return PlanEnterprise
	case "claude_team":
		return PlanTeam
	}

	switch {
	case p.Account.HasClaudeMax:
		return PlanMax
	case p.Account.HasClaudePro:
		return PlanPro
	case p.Organization.Type != "":
		return PlanUnknown
	}

	return PlanFree
}

// UsageBased reports whether the seat bills by usage against a credit pool,
// as opposed to a fixed allowance.
func (p Profile) UsageBased() bool {
	return p.Organization.SeatTier == "enterprise_usage_based"
}

func profilePath() (string, error) {
	dir, err := accountDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, "profile.json"), nil
}

// credentialFile is where the fingerprint of the credentials a cached profile
// was fetched with is recorded.
const credentialFile = "profile.credential"

func credentialPath() (string, error) {
	dir, err := accountDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, credentialFile), nil
}

// cachedCredential returns the fingerprint recorded with the cached profile.
// It is empty when there is none - the case for a cache written before this
// was recorded - which counts as a mismatch and costs one fetch.
func cachedCredential() string {
	path, err := credentialPath()
	if err != nil {
		return ""
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(raw))
}

// matchesCredentials reports whether the cached profile was fetched with the
// credentials in force now.
//
// It answers true when no fingerprint can be computed: with no credentials to
// compare against there is nothing to fetch with either, so serving the cache
// beats failing outright.
func matchesCredentials() bool {
	current, err := credentialFingerprint()
	if err != nil {
		return true
	}

	return cachedCredential() == current
}

// recordCredential notes which credentials the cache now on disk was fetched
// with. It runs after that cache is in place: a fingerprint written first
// would vouch for the profile it replaced, while one written second can only
// ever cost an extra fetch.
func recordCredential() error {
	fingerprint, err := credentialFingerprint()
	if err != nil {
		return err
	}

	path, err := credentialPath()
	if err != nil {
		return err
	}

	err = os.WriteFile(path, []byte(fingerprint), filePerms)
	if err != nil {
		return fmt.Errorf("writing profile credential: %w", err)
	}

	return nil
}

// LoadProfile returns the cached profile, fetching it when there is none or it
// has gone stale. Unlike usage this is allowed to block: it is called by the
// plan command, not on the render path.
func LoadProfile() (*Profile, error) {
	path, err := profilePath()
	if err != nil {
		return nil, err
	}

	info, statErr := os.Stat(path)
	if statErr != nil || time.Since(info.ModTime()) > ProfileTTL || !matchesCredentials() {
		err = RefreshProfile()
		if err != nil {
			return nil, err
		}

		info, statErr = os.Stat(path)
		if statErr != nil {
			return nil, fmt.Errorf("no cached profile: %w", statErr)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading profile cache: %w", err)
	}

	var profile Profile

	err = json.Unmarshal(raw, &profile)
	if err != nil {
		return nil, fmt.Errorf("parsing profile cache: %w", err)
	}

	profile.Age = time.Since(info.ModTime())

	return &profile, nil
}

// RefreshProfile fetches the profile from Anthropic and writes the cache.
func RefreshProfile() error {
	token, err := accessToken()
	if err != nil {
		return err
	}

	body, retry, err := get(token, "profile")
	if err != nil {
		setBackoff(retry)

		return err
	}

	path, err := profilePath()
	if err != nil {
		return err
	}

	// Write-then-rename, so a reader never sees a half-written cache.
	tmp := path + ".tmp"

	err = os.WriteFile(tmp, body, filePerms)
	if err != nil {
		return fmt.Errorf("writing profile cache: %w", err)
	}

	err = os.Rename(tmp, path)
	if err != nil {
		return fmt.Errorf("replacing profile cache: %w", err)
	}

	return recordCredential()
}

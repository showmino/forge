package store

import (
	"context"
	"time"
)

// Release is what every part of an application ran in production after
// one promotion or rollback of the application.
type Release struct {
	Application string
	// Number counts from 1 for each application; AddRelease sets it.
	Number    int
	CreatedAt time.Time
	CreatedBy string
	// Note is empty for a promotion and says what a rollback returned to.
	Note  string
	Parts []ReleasePart
}

// ReleasePart is what one part ran in a release.
type ReleasePart struct {
	Service      string `json:"service"`
	Version      string `json:"version,omitempty"`
	Commit       string `json:"commit,omitempty"`
	DeployCommit string `json:"deployCommit,omitempty"`
}

// ReleaseStore keeps the releases of applications.
type ReleaseStore interface {
	// AddRelease records r as the application's next release and returns
	// its number.
	AddRelease(ctx context.Context, r Release) (int, error)
	// Releases returns up to limit releases of application, newest first.
	Releases(ctx context.Context, application string, limit int) ([]Release, error)
}

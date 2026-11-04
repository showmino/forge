package github

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Pull is a pull request, as far as Forge reads one.
type Pull struct {
	Number  int
	HTMLURL string
	// Merged is when it was merged; zero when it was not.
	Merged time.Time
	// Base is the branch it merges into, HeadSHA its last commit, and
	// MergeCommit the commit its merge made on Base.
	Base, HeadSHA, MergeCommit string
}

// Review is one review of a pull request.
type Review struct {
	// User is the reviewer's login.
	User string
	// State is APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED or PENDING.
	State string
	// CommitID is the commit the review was given on.
	CommitID string
}

// PullsForCommit returns the pull requests a commit of owner/repo belongs to.
func (c *Client) PullsForCommit(ctx context.Context, owner, repo, sha string) ([]Pull, error) {
	var out []struct {
		Number   int        `json:"number"`
		HTMLURL  string     `json:"html_url"`
		MergedAt *time.Time `json:"merged_at"`
		Base     struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		MergeCommitSHA string `json:"merge_commit_sha"`
	}
	if err := c.doJSON(ctx, http.MethodGet, repoPath(owner, repo)+"/commits/"+sha+"/pulls", nil, &out); err != nil {
		return nil, err
	}
	pulls := make([]Pull, 0, len(out))
	for _, p := range out {
		pl := Pull{Number: p.Number, HTMLURL: p.HTMLURL, Base: p.Base.Ref, HeadSHA: p.Head.SHA, MergeCommit: p.MergeCommitSHA}
		if p.MergedAt != nil {
			pl.Merged = *p.MergedAt
		}
		pulls = append(pulls, pl)
	}
	return pulls, nil
}

// PullReviews returns the reviews of pull request number of owner/repo,
// oldest first (at most 300).
func (c *Client) PullReviews(ctx context.Context, owner, repo string, number int) ([]Review, error) {
	var reviews []Review
	for page := 1; page <= 3; page++ {
		var out []struct {
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			State    string `json:"state"`
			CommitID string `json:"commit_id"`
		}
		path := fmt.Sprintf("%s/pulls/%d/reviews?per_page=100&page=%d", repoPath(owner, repo), number, page)
		if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		for _, r := range out {
			reviews = append(reviews, Review{User: r.User.Login, State: r.State, CommitID: r.CommitID})
		}
		if len(out) < 100 {
			break
		}
	}
	return reviews, nil
}

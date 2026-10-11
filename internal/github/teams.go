package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Team is a team of a GitHub organization.
type Team struct {
	// Slug is the name in URLs and in a login's groups, for example "platform-engineering".
	Slug string
	// Name is the name as people wrote it, for example "Platform Engineering".
	Name        string
	Description string
}

// ListTeams returns every team of org, ordered by slug. A GitHub App needs
// the organization permission "Members: read" for it; without it GitHub
// answers 403.
func (c *Client) ListTeams(ctx context.Context, org string) ([]Team, error) {
	var teams []Team
	for page := 1; page <= maxPages; page++ {
		resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/orgs/%s/teams?per_page=%d&page=%d", url.PathEscape(org), pageSize, page), nil)
		if err != nil {
			return nil, err
		}
		var batch []struct {
			Slug        string `json:"slug"`
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		err = json.NewDecoder(resp.Body).Decode(&batch)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode teams of %s: %w", org, err)
		}
		for _, t := range batch {
			teams = append(teams, Team{Slug: t.Slug, Name: t.Name, Description: t.Description})
		}
		if len(batch) < pageSize {
			break
		}
	}
	slices.SortFunc(teams, func(a, b Team) int { return strings.Compare(a.Slug, b.Slug) })
	return teams, nil
}

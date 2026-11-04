package repositories

// AdoptionPath is the explicit, reviewed record for a repo whose create
// event predates its current catalog entry.
func AdoptionPath(repo string) string { return "repositories/" + repo + "/adoption.yaml" }

// ReviewExceptionPath is the deploy repo record for a reviewed alternate
// code-owner team.
func ReviewExceptionPath(repo string) string { return "review-exceptions/" + repo + ".yaml" }

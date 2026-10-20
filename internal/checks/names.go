package checks

// Info is how a check is presented to people. Name stays the machine name
// (it is the commit status context, which branch protection may require).
type Info struct {
	Name  string
	Label string
	About string // what it looks at, in one sentence
	Fix   string // what a developer does when it fails
}

var catalog = []Info{
	{
		Name: "pr-rules", Label: "Conventions",
		About: "Title format, description and a linked issue, as set in .repoguard.yml.",
		Fix:   "Edit the title or description of the merge request. No new commit is needed; the check runs again on its own.",
	},
	{
		Name: "secrets", Label: "Secrets",
		About: "Passwords, tokens and private keys in the added lines.",
		Fix:   "Remove the value from the code, rotate it where it was issued, and load it from configuration or an environment variable.",
	},
	{
		Name: "ai-review", Label: "AI Review",
		About: "Claude reads the diff and comments on likely bugs and risky changes.",
		Fix:   "Read the inline comments. They are advice; resolve them or reply on the thread.",
	},
	{
		Name: "ci", Label: "Build & Tests",
		About: "Install, lint, build and test every project the change touches, plus secret, dependency, API and SonarQube checks.",
		Fix:   "Open the failing step below, fix it locally with the same command, and push a new commit.",
	},
}

// Lookup returns the presentation of a check; unknown names fall back to the name.
func Lookup(name string) Info {
	for _, i := range catalog {
		if i.Name == name {
			return i
		}
	}
	return Info{Name: name, Label: name}
}

func Label(name string) string { return Lookup(name).Label }

// Order sorts checks the same way everywhere.
func Order(name string) int {
	for i, c := range catalog {
		if c.Name == name {
			return i
		}
	}
	return len(catalog)
}

// Catalog lists every known check in display order.
func Catalog() []Info { return append([]Info(nil), catalog...) }

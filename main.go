package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/google/go-github/v35/github"
	"golang.org/x/mod/semver"
	"golang.org/x/oauth2"
)

// 1. Determine latest version (enum tags?)
// 2. Fetch commits since latest tag
// 3. Calculate version based on commits
// 4. Create tag
// 5. Create release notes
// 6. Create release

type Versions []string

func (v Versions) Len() int {
	return len(v)
}

func (v Versions) Less(i, j int) bool {
	return semver.Compare(v[i], v[j]) > 0
}

func (v Versions) Swap(i, j int) {
	v[i], v[j] = v[j], v[i]
}

func abort(f string, args ...interface{}) {
	_, _ = fmt.Fprintf(os.Stderr, "- %s\n", fmt.Sprintf(f, args...))
	os.Exit(1)
}

func info(f string, args ...interface{}) {
	fmt.Printf("+ %s\n", fmt.Sprintf(f, args...))
}

func warn(f string, args ...interface{}) {
	fmt.Printf("! %s\n", fmt.Sprintf(f, args...))
}

func main() {
	repoPath := os.Getenv("GITHUB_WORKSPACE")
	repoFullName := os.Getenv("GITHUB_REPOSITORY")
	repoComponents := strings.Split(repoFullName, "/")
	repoOwner, repoName := repoComponents[0], repoComponents[1]
	token := os.Args[1]

	info("Working on %s", repoPath)
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		abort("Could not open %s: %s", repoPath, err)
	}

	// Determine latest version
	allTags, err := repo.Tags()
	if err != nil {
		abort("Could not enumerate tags: %s", err)
	}

	var tags Versions
	err = allTags.ForEach(func(ref *plumbing.Reference) error {
		if semver.IsValid(ref.Name().Short()) {
			tags = append(tags, ref.Name().Short())
		}
		return nil
	})

	if err != nil {
		abort("Could not iterate tags: %s", err)
	}

	sort.Sort(tags)

	var latestVersion string
	if tags.Len() > 0 {
		latestVersion = tags[0]
	}

	var head plumbing.Hash
	if latestVersion != "" {
		info("Latest version is %s", latestVersion)
		tag, err := repo.Tag(latestVersion)
		if err != nil {
			abort("Could not read tag %s: %s", latestVersion, err)
		}
		head = tag.Hash()
	} else {
		warn("No SemVer tag found. Assuming as first release...")
		var lastCommit *object.Commit
		log, err := repo.Log(&git.LogOptions{})
		if err != nil {
			abort("Error enumerating commits: %s", err)
		}

		for {
			c, err := log.Next()
			if err == io.EOF {
				break
			}
			lastCommit = c
		}

		if lastCommit == nil {
			abort("Repository does not have a commit")
			return
		}

		head = lastCommit.Hash
		latestVersion = "v0.0.0"
	}

	info("Using commits since %s", head)
	commits, err := repo.Log(&git.LogOptions{})
	if err != nil {
		abort("Error reading commits: %s", err)
	}

	var conventionals Commits
	for {
		commit, err := commits.Next()
		if err != nil {
			abort("Error iterating commits: %s", err)
		}
		if commit.Hash == head {
			break
		}
		if conv := ParseCommit(strings.TrimSpace(commit.Message)); conv != nil {
			conventionals = append(conventionals, conv)
		}
	}
	commits.Close()

	if len(conventionals) == 0 {
		info("No new commits to release.")
		return
	}

	major, minor, patch := parseSemVer(latestVersion)
	switch conventionals.ChangeKind() {
	case SemVerPatch:
		patch++
	case SemVerMinor:
		patch = 0
		minor++
	case SemVerMajor:
		patch = 0
		minor = 0
		major++
	}

	nextVersion := fmt.Sprintf("v%d.%d.%d", major, minor, patch)
	breaks, feats, fixes := conventionals.Stats()
	info("Releasing %s with %d break(s), %d feature(s), %d fix(es)", nextVersion, breaks, feats, fixes)

	currentHead, err := repo.Head()
	if err != nil {
		abort("Error reading head: %s", err)
	}

	tag, err := repo.CreateTag(nextVersion, currentHead.Hash(), nil)
	if err != nil {
		abort("Error tagging %s: %s", nextVersion, err)
	}

	info("Created tag %s", tag.Hash())

	remoteName := "__semver_releaser_http"
	// Create a random remote and push to it
	if _, err = repo.Remote(remoteName); err == git.ErrRemoteNotFound {
		info("Created helper remote %s", remoteName)
		_, err = repo.CreateRemote(&config.RemoteConfig{
			Name: remoteName,
			URLs: []string{"https://github.com/" + repoFullName + ".git"},
		})
	}

	err = repo.Push(&git.PushOptions{
		RemoteName: remoteName,
		RefSpecs: []config.RefSpec{
			config.RefSpec("+refs/tags/" + nextVersion + ":refs/tags/" + nextVersion),
		},
		Auth: &http.BasicAuth{
			Username: "x-access-token",
			Password: token,
		},
	})

	if err != nil {
		abort("Error pushing: %s", err)
	}

	info("Pushed tag")
	releaseText := makeRelease(conventionals)

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctx, ts)

	client := github.NewClient(tc)
	_, _, err = client.Repositories.CreateRelease(ctx, repoOwner, repoName, &github.RepositoryRelease{
		TagName:    github.String(nextVersion),
		Name:       github.String(nextVersion),
		Body:       github.String(releaseText),
		Draft:      github.Bool(false),
		Prerelease: github.Bool(false),
	})

	if err != nil {
		abort("Error creating release: %s", err)
	}
}

func formatCommit(c *ConventionalCommit) string {
	if c.Scope != "" {
		return fmt.Sprintf("- **%s**: %s", c.Scope, c.Description)
	} else {
		return fmt.Sprintf("- %s", c.Description)
	}
}

func makeRelease(conventionals Commits) string {
	var feats []string
	var fixes []string
	var others []string

	for _, c := range conventionals {
		switch c.Change {
		case ConventionalFeature:
			feats = append(feats, formatCommit(c))
		case ConventionalFix:
			fixes = append(fixes, formatCommit(c))
		default:
			if c.Scope != "" {
				others = append(others, fmt.Sprintf("- %s(%s): %s", c.Type, c.Scope, c.Description))
			} else {
				others = append(others, fmt.Sprintf("- %s: %s", c.Type, c.Description))
			}
		}
	}

	var output []string

	if len(feats) > 0 {
		output = append(output, "# ✨ New Features")
		output = append(output, feats...)
	}

	if len(fixes) > 0 {
		output = append(output, "# 🐞 Bug Fixes")
		output = append(output, fixes...)
	}

	if len(others) > 0 {
		output = append(output, "# 🔨 Other Changes")
		output = append(output, others...)
	}

	return strings.Join(output, "\n")
}

func parseSemVer(v string) (major, minor, patch int) {
	rawComponents := strings.Split(strings.TrimPrefix(v, "v"), ".")
	major, _ = strconv.Atoi(rawComponents[0])
	minor, _ = strconv.Atoi(rawComponents[1])
	patch, _ = strconv.Atoi(rawComponents[2])
	return
}

type ConventionalChange int

const (
	ConventionalOther ConventionalChange = iota
	ConventionalFix
	ConventionalFeature
)

type SemVerComponent int

const (
	SemVerNone SemVerComponent = iota
	SemVerPatch
	SemVerMinor
	SemVerMajor
)

type ConventionalCommit struct {
	Type         string
	Change       ConventionalChange
	SemVerChange SemVerComponent
	Scope        string
	Description  string
	Body         string
}

type Commits []*ConventionalCommit

func (c Commits) ChangeKind() SemVerComponent {
	m := map[SemVerComponent]bool{}
	for _, c := range c {
		m[c.SemVerChange] = true
	}

	if _, ok := m[SemVerMajor]; ok {
		return SemVerMajor
	}
	if _, ok := m[SemVerMinor]; ok {
		return SemVerMinor
	}
	if _, ok := m[SemVerPatch]; ok {
		return SemVerPatch
	}
	return SemVerNone
}

func (c Commits) Stats() (breaks, feats, fixes int) {
	for _, c := range c {
		switch c.SemVerChange {
		case SemVerPatch:
			fixes++
		case SemVerMinor:
			feats++
		case SemVerMajor:
			breaks++
		}
	}
	return
}

var conventionalRegexp = regexp.MustCompile(`^([^(:!]+)(?:\(([^)]+)\))?(!)?: ([^\n]+)$`)
var multiLineCommit = regexp.MustCompile(`(.+)\n{2,}(.+\n*)+`)

func ParseCommit(msg string) *ConventionalCommit {
	if multiLineCommit.MatchString(msg) {
		lines := strings.Split(msg, "\n")
		res := ParseCommit(lines[0])
		if res == nil {
			return nil
		}
		for _, l := range lines[1:] {
			if strings.HasPrefix(strings.ToLower(l), "breaking change:") {
				res.SemVerChange = SemVerMajor
			}
		}
		res.Body = strings.Join(lines[1:], "\n")

		return res
	}
	if !conventionalRegexp.MatchString(msg) {
		return nil
	}

	opts := conventionalRegexp.FindStringSubmatch(msg)
	var kind, scope, bang, change = opts[1], opts[2], opts[3], opts[4]
	res := &ConventionalCommit{
		Type:        kind,
		Scope:       scope,
		Description: change,
		Body:        "",
	}
	switch strings.ToLower(kind) {
	case "fix":
		res.SemVerChange = SemVerPatch
		res.Change = ConventionalFix
	case "feat":
		res.SemVerChange = SemVerMinor
		res.Change = ConventionalFeature
	}
	if bang == "!" {
		res.SemVerChange = SemVerMajor
	}

	return res
}

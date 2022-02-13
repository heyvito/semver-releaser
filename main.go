package main

import (
	"context"
	"fmt"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/google/go-github/v39/github"
	"github.com/heyvito/semver-releaser/eql"
	"github.com/urfave/cli/v2"
	"golang.org/x/mod/semver"
	"golang.org/x/oauth2"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 1. Determine the latest version (enum tags?)
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

type Context struct {
	Token      string
	Push       bool
	Rules      map[string]string
	Categories map[string]string
	Ignore     []string
}

func run(c *Context) {
	repoPath := os.Getenv("GITHUB_WORKSPACE")
	repoFullName := os.Getenv("GITHUB_REPOSITORY")
	repoComponents := strings.Split(repoFullName, "/")
	repoOwner, repoName := repoComponents[0], repoComponents[1]

	for _, r := range c.Rules {
		r = strings.ToLower(r)
		if r != "patch" && r != "minor" && r != "major" {
			fmt.Printf("Error: Invalid semver component '%s'", r)
			os.Exit(1)
		}
	}

	runInfo := []string{
		"semver-releaser v2",
		"https://github.com/heyvito/semver-releaser",
		"",
		"Run information",
		"---------------",
		fmt.Sprintf("Working on %s", repoPath),
		fmt.Sprintf("Will push changes? %t", c.Push),
		fmt.Sprintf("Rules"),
		fmt.Sprintf("-----"),
	}

	for k, v := range c.Rules {
		runInfo = append(runInfo, fmt.Sprintf("    '%s' commits bumps %s", strings.ToLower(k), v))
	}

	runInfo = append(runInfo, "",
		"When writing release notes...")

	for k, v := range c.Categories {
		if k == "*" {
			continue
		}
		runInfo = append(runInfo, fmt.Sprintf("    ...group all '%s' commits under '%s';", strings.ToLower(k), v))
	}

	if title, ok := c.Categories["*"]; ok {
		runInfo = append(runInfo, fmt.Sprintf("    ...and all other commits under '%s';", title))
	}

	if len(c.Ignore) > 0 {
		runInfo = append(runInfo, "",
			"Ignore commits with the following prefixes:")
		for _, v := range c.Ignore {
			runInfo = append(runInfo, " - "+v)
		}
	}

	fmt.Println(strings.Join(runInfo, "\n"))
	fmt.Println()

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

	info("Last release is at %s", head)
	commits, err := repo.Log(&git.LogOptions{
		All: true,
	})
	if err != nil {
		abort("Error reading commits: %s", err)
	}

	excluded := 0
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
			for _, v := range c.Ignore {
				if strings.ToLower(v) == conv.Type {
					excluded++
					continue
				}
			}
			conventionals = append(conventionals, conv)
		} else {
			warn("Ignoring non-standard commit: %s", strings.Split(commit.Message, "\n")[0])
		}
	}
	commits.Close()

	if len(conventionals) == 0 {
		info("No new commits to release.")
		return
	}

	info("Processing %d commit(s) since %s", len(conventionals), head)
	if excluded > 0 {
		info("%d commit(s) matched the 'ignore' flag and were excluded", excluded)
	}

	major, minor, patch := parseSemVer(latestVersion)
	bumpKind := determineBump(c, conventionals)
	switch bumpKind {
	case SemVerPatch:
		patch++
	case SemVerMinor:
		patch = 0
		minor++
	case SemVerMajor:
		patch = 0
		minor = 0
		major++
	case SemVerNone:
		info("No need to bump version.")
		return
	}

	nextVersion := fmt.Sprintf("v%d.%d.%d", major, minor, patch)
	info("Releasing %s", nextVersion)

	fmt.Printf("::set-output name=version::%s\n", nextVersion)

	if !c.Push {
		return
	}

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
			Password: c.Token,
		},
	})

	if err != nil {
		abort("Error pushing: %s", err)
	}

	info("Pushed tag")
	releaseText := makeReleaseText(c, conventionals)

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: c.Token})
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

var semverString = map[string]SemVerComponent{
	"patch": SemVerPatch,
	"minor": SemVerMinor,
	"major": SemVerMajor,
}

func semverFromString(n string) SemVerComponent {
	n = strings.TrimSpace(strings.ToLower(n))
	if v, ok := semverString[n]; ok {
		return v
	}

	return SemVerNone
}

func determineBump(c *Context, commits Commits) SemVerComponent {
	bang := SemVerNone
	_, hasBang := c.Rules["bang"]
	components := map[SemVerComponent][]string{}
	toBump := SemVerNone

	for ruleName, kind := range c.Rules {
		if ruleName == "bang" {
			bang = semverFromString(kind)
			continue
		}

		k := semverFromString(kind)
		components[k] = append(components[k], ruleName)
	}

	comps := []SemVerComponent{SemVerMajor, SemVerMinor, SemVerPatch}

	for _, r := range commits {
		if toBump == SemVerMajor {
			break
		}

		if r.Bang && hasBang {
			if bang > toBump {
				toBump = bang
				continue
			}
		}

		prefix := strings.ToLower(r.Type)
	compLoop:
		for _, v := range comps {
			prefixes, ok := components[v]
			if !ok {
				continue
			}
			if toBump > v {
				continue
			}

			for _, pr := range prefixes {
				if strings.ToLower(pr) == prefix {
					toBump = v
					break compLoop
				}
			}
		}
	}

	return toBump
}

func formatCommit(c *ConventionalCommit) string {
	if c.Scope != "" {
		return fmt.Sprintf("- **%s**: %s", c.Scope, c.Description)
	} else {
		return fmt.Sprintf("- %s", c.Description)
	}
}

func makeReleaseText(c *Context, commits Commits) string {
	categories := map[string][]string{}
	usesOther := false
	var others []string
	for cat := range c.Categories {
		if cat == "*" {
			usesOther = true
			break
		}
	}

	for _, r := range commits {
		commitType := strings.ToLower(r.Type)
		match := false
		for cat := range c.Categories {
			if cat == "*" {
				continue
			}
			if commitType == strings.ToLower(cat) {
				var arr []string
				if v, ok := categories[cat]; ok {
					arr = v
				}
				categories[cat] = append(arr, formatCommit(r))
				match = true
				break
			}
		}

		if !match && usesOther {
			if r.Scope != "" {
				others = append(others, fmt.Sprintf("- %s(%s): %s", r.Type, r.Scope, r.Description))
			} else {
				others = append(others, fmt.Sprintf("- %s: %s", r.Type, r.Description))
			}
		}
	}

	var output []string

	for id, title := range c.Categories {
		if items, ok := categories[strings.ToLower(id)]; ok {
			output = append(output, fmt.Sprintf("# %s", title))
			output = append(output, items...)
		}
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

type SemVerComponent int

const (
	SemVerNone SemVerComponent = iota
	SemVerPatch
	SemVerMinor
	SemVerMajor
)

type ConventionalCommit struct {
	Type         string
	SemVerChange SemVerComponent
	Scope        string
	Description  string
	Body         string
	Bang         bool
}

type Commits []*ConventionalCommit

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
				res.Bang = true
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
		Bang:        bang == "!",
		Body:        "",
	}

	return res
}

func main() {
	app := cli.App{
		Name: "semver-releaser",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "token", Required: true},
			&cli.StringFlag{Name: "push", Required: false, Value: "true"},
			&cli.StringFlag{Name: "rules", Required: true},
			&cli.StringFlag{Name: "categories", Required: true},
			&cli.StringFlag{Name: "ignore", Required: false},
		},
		Action: func(c *cli.Context) error {
			rules, err := eql.Parse(c.String("rules"))
			if err != nil {
				fmt.Printf("Error parsing rules: %s\n", err)
				os.Exit(1)
			}
			cats, err := eql.Parse(c.String("categories"))
			if err != nil {
				fmt.Printf("Error parsing categories: %s\n", err)
				os.Exit(1)
			}

			rawIgnore := strings.TrimSpace(c.String("ignore"))
			var ignore []string
			if len(rawIgnore) > 0 {
				ignore = strings.Split(rawIgnore, " ")
			}

			ctx := Context{
				Token:      c.String("token"),
				Push:       c.String("push") == "true",
				Rules:      rules,
				Categories: cats,
				Ignore:     ignore,
			}

			run(&ctx)
			return nil
		},
	}

	if err := app.Run(os.Args); err != nil {
		panic(err)
	}
}

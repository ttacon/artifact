package builder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/ttacon/chalk"
	"github.com/urfave/cli/v2"
)

var (
	warn = chalk.Yellow.NewStyle().WithTextStyle(chalk.Bold).Style
)

// findValueFromEnv is our cheap way to try to identify a value from potentially
// many environment variables. Why do we do this? Because differnt build systems
// autopopulate very different envvars (e.g. CircleCI, Jenkins, Travis, GitHub
// actions, etc).
//
// findValueFromEnv will loop through the given list, and return the first
// identified value, or false if no envvar had a value. Note that this is
// looking for content, not if an envvar is set.
func findValueFromEnv(keys []string) (string, bool) {
	for _, key := range keys {
		if val := os.Getenv(key); len(val) > 0 {
			return val, true
		}
	}
	return "", false
}

var (
	// Envvars that tell us what the last known git reference was.
	gitRangeStartEnvvars = []string{
		"ARTIFACT_GIT_RANGE_START",
		"GIT_PREVIOUS_COMMIT",
	}
	// Envvars that tell us what the new git reference is.
	gitRangeEndEnvvars = []string{
		"ARTIFACT_GIT_RANGE_END",
		"GIT_COMMIT",
	}
)

// getGitRangeVal is a helper function for identifying the desired git range
// value from either a flag to the CLI or an envvar, in that order of
// precedence. If none is found, the empty string is returned.
func getGitRangeVal(c *cli.Context, key string, envNames []string) string {
	if val := c.String(key); len(val) > 0 {
		return val
	} else if val, ok := findValueFromEnv(envNames); ok {
		return val
	}
	return ""
}

// Builder is our primary interface or interacting with artifact. It determines
// which actions to load and how to execute them, passing a context between all
// actions as they execute.
type Builder interface {
	Run() ([]string, error)
}

// builder is our local implementation of the `Builder` interface, it is a set
// of ordered `Action`s to take.
type builder struct {
	actions []Action
}

// NewBuilderFromCLI creates a builder from a CLI's context.
func NewBuilderFromCLI(c *cli.Context) Builder {
	isDryRun := c.Bool("dry-run")
	if isDryRun {
		log.Println(warn("this is a dry run, no changes will be made"))
	}

	var actions []Action

	var workingDir = c.String("working-directory")
	if len(workingDir) > 0 {
		log.Println("different working directory specific, will be working in: ", workingDir)
	}

	// Identify changeset
	actions = append(actions, &ChangesetIdentification{
		GitRangeStart:    getGitRangeVal(c, "git-range-start", gitRangeStartEnvvars),
		GitRangeEnd:      getGitRangeVal(c, "git-range-end", gitRangeEndEnvvars),
		WorkingDirectory: workingDir,
	})

	// Identify artifact entrypoints
	actions = append(actions, &EntrypointIdentification{
		Prefix:           c.String("cmd-prefix"),
		SkipNested:       c.Bool("skip-nested-entrypoints"),
		WorkingDirectory: workingDir,
	})

	// Identify dependencies of entrypoints
	actions = append(actions, &EntrypointDependencyIdentification{
		RepoBasename:     c.String("repo-basename"),
		WorkingDirectory: workingDir,
	})

	// Determine targets that must be rebuilt
	actions = append(actions, ModifiedDependencies{})

	// Identify any entrypoints that need to be rebuilt
	actions = append(actions, &OutputDependencies{
		Format: c.String("out-format"),
	})

	// Rebuild these artifacts
	actions = append(actions, &RebuildTargets{
		IsDryRun:         isDryRun,
		BuildCommand:     c.String("build-command"),
		WorkingDirectory: workingDir,
	})

	// TODO(ttacon): add an action for persisting the build logs to disk.

	return &builder{
		actions: actions,
	}
}

// Run runs our actions sequentially, passing a continually evolving context
// between them.
func (b *builder) Run() ([]string, error) {
	ctx := context.TODO()
	for _, action := range b.actions {
		if err := action.Precheck(ctx); err != nil {
			return nil, err
		}

		newCtx, err := action.Do(ctx)
		if err != nil {
			return nil, err
		}

		ctx = newCtx
	}

	rebuilds, ok := ctx.Value("rebuilds").([]string)
	if !ok {
		return nil, errors.New("no valid targets to rebuild determined")
	}

	return rebuilds, nil
}

type Action interface {
	Precheck(ctx context.Context) error
	Do(ctx context.Context) (context.Context, error)
}

type ChangesetIdentification struct {
	GitRangeStart string
	GitRangeEnd   string

	WorkingDirectory string
}

var ErrInvalidGitRange = errors.New("invalid git range")

func (c *ChangesetIdentification) Precheck(_ context.Context) error {
	if len(c.GitRangeStart) == 0 || len(c.GitRangeEnd) == 0 {
		return ErrInvalidGitRange
	}
	return nil
}

func (c *ChangesetIdentification) Do(ctx context.Context) (context.Context, error) {
	log.Printf(`running: "git diff-tree --no-commit-id --name-only -r %s..%s"\n`, c.GitRangeEnd, c.GitRangeStart)
	cmd := exec.Command(
		"git",
		"diff-tree",
		"--no-commit-id",
		"--name-only",
		"-r",
		fmt.Sprintf("%s..%s", c.GitRangeEnd, c.GitRangeStart),
	)
	if len(c.WorkingDirectory) > 0 {
		cmd.Dir = c.WorkingDirectory
	}

	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	changes := strings.Split(string(out), "\n")

	log.Println("identified changes: ", changes)
	return context.WithValue(ctx, "changes", changes), nil
}

type EntrypointIdentification struct {
	Prefix           string
	SkipNested       bool
	WorkingDirectory string
}

var ErrNoChangesFound = errors.New("no changes were found")

func (c *EntrypointIdentification) Precheck(ctx context.Context) error {
	// Check our assumptions of the previous steps work.
	changes, ok := ctx.Value("changes").([]string)
	if !ok {
		return errors.New("failed to find identified changesets")
	} else if len(changes) == 0 {
		return ErrNoChangesFound
	}

	pathOfInterest := filepath.Join(c.WorkingDirectory, c.Prefix)

	// Ensure that our path exists.
	dir, err := os.Open(pathOfInterest)
	if err != nil {
		return err
	} else if fInfo, err := dir.Stat(); err != nil {
		return err
	} else if !fInfo.IsDir() {
		return errors.New("provided prefix must point to  directory")
	}
	return nil
}

func (c *EntrypointIdentification) Do(ctx context.Context) (context.Context, error) {
	log.Printf("opening prefixed path: %q, will do nested check: %v\n", c.Prefix, !c.SkipNested)

	pathOfInterest := filepath.Join(c.WorkingDirectory, c.Prefix)

	if c.SkipNested {
		// Simply return the path, we already know that it exists.
		return context.WithValue(ctx, "entrypoints", []string{pathOfInterest}), nil
	}

	log.Println("opening directory")
	dir, err := os.Open(pathOfInterest)
	if err != nil {
		return nil, err
	}

	log.Println("reading directory")
	entries, err := dir.Readdir(-1)
	if err != nil {
		return nil, err
	}

	var entrypoints []string
	for _, entry := range entries {
		log.Println("inspecting entry: ", entry.Name())
		if entry.IsDir() {
			entrypoints = append(
				entrypoints,
				filepath.Join(c.Prefix, entry.Name()),
			)
		}
	}

	log.Println("passing on entrypoints: ", strings.Join(entrypoints, ", "))
	return context.WithValue(ctx, "entrypoints", entrypoints), nil
}

type EntrypointDependencyIdentification struct {
	RepoBasename     string
	WorkingDirectory string
}

func (e *EntrypointDependencyIdentification) Precheck(_ context.Context) error {
	// TODO(ttacon): determine if we want to do more than solely validate
	// that this exists (e.g. it looks like a golang package/module base,
	// github.com/foo/bar).
	return nil
}

func (e *EntrypointDependencyIdentification) Do(ctx context.Context) (context.Context, error) {
	log.Println("beginning entrypoint dependency identification, repo basename is: ", e.RepoBasename)

	entrypoints, ok := ctx.Value("entrypoints").([]string)
	if !ok {
		return nil, errors.New("no valid entrypoints were provided from previous step")
	}

	var entryMap = make(map[string][]string)

	for _, entrypoint := range entrypoints {
		cmd := exec.Command(
			"go",
			"list",
			"-f",
			`'{{ join .Deps "\n" }}'`,
			fmt.Sprintf("./%s", entrypoint),
		)
		cmd.Dir = e.WorkingDirectory

		out, err := cmd.Output()
		if err != nil {
			return nil, err
		}

		deps := strings.Split(string(out), "\n")
		var depsOfInterest []string
		for _, dep := range deps {
			if strings.HasPrefix(dep, e.RepoBasename) {
				log.Printf("for entrypoint %q, found dep %q\n", entrypoint, dep)
				depsOfInterest = append(
					depsOfInterest,
					strings.TrimPrefix(dep, e.RepoBasename+"/"),
				)
			}
		}

		if len(depsOfInterest) > 0 {
			entryMap[entrypoint] = depsOfInterest
		}
	}

	return context.WithValue(ctx, "dependencies", entryMap), nil
}

type ModifiedDependencies struct{}

func (m ModifiedDependencies) Precheck(_ context.Context) error {
	return nil
}

func (e ModifiedDependencies) Do(ctx context.Context) (context.Context, error) {
	changes, ok := ctx.Value("changes").([]string)
	if !ok {
		return nil, errors.New("no valid changes were identified")
	}

	log.Println("building set of known changes")
	var changeSet = make(map[string]struct{})
	for _, change := range changes {
		changeSet[path.Dir(change)] = struct{}{}
	}

	dependencies, ok := ctx.Value("dependencies").(map[string][]string)
	if !ok {
		return nil, errors.New("no valid dependencies were identified")
	}

	var toBeRebuilt []string
	for entrypoint, deps := range dependencies {
		log.Printf("checking dependencies for entrypoint: %q\n", entrypoint)
		for _, dep := range deps {
			if _, ok := changeSet[dep]; ok {
				log.Printf("identified matching change: %q\n", dep)
				toBeRebuilt = append(toBeRebuilt, entrypoint)
				break
			}
		}
	}

	numTargets := len(toBeRebuilt)
	log.Printf("identified %d targets to be rebuilt\n", numTargets)
	if numTargets == 0 {
		return nil, errors.New("no targets need to be rebuilt")
	}

	for i, target := range toBeRebuilt {
		log.Printf("[%d/%d] target to be rebuilt: %q\n", i, numTargets, target)
	}

	return context.WithValue(ctx, "targets", toBeRebuilt), nil
}

type OutputDependencies struct {
	Format string
}

var (
	validOutputFormats = map[string]struct{}{
		"txt":  struct{}{},
		"json": struct{}{},
	}

	ErrInvalidOutputFormat = errors.New("invalid output format")
)

func (m *OutputDependencies) Precheck(_ context.Context) error {
	if _, ok := validOutputFormats[m.Format]; !ok {
		return ErrInvalidOutputFormat
	}
	return nil
}

func (e *OutputDependencies) Do(ctx context.Context) (context.Context, error) {
	targets, ok := ctx.Value("targets").([]string)
	if !ok {
		return nil, errors.New("new valid targets identified")
	}

	// Hmm, I don't really like this? This action should likely be an
	// optional, terminal action.
	log.Println("output format is: ", e.Format)
	if e.Format == "json" {
		data, err := json.Marshal(targets)
		if err != nil {
			return nil, err
		}
		fmt.Println(string(data))
		return nil, nil
	}

	// Otherwise, it's the `txt` format.
	log.Println("targets: ", strings.Join(targets, ", "))
	return context.WithValue(
		ctx,
		"rebuilds",
		targets,
	), nil
}

type RebuildTargets struct {
	IsDryRun         bool
	BuildCommand     string
	WorkingDirectory string
}

func (r *RebuildTargets) Precheck(_ context.Context) error {
	if len(r.BuildCommand) == 0 {
		return errors.New("must provide build command")
	} else if !strings.Contains(r.BuildCommand, "{{entrypoint}}") {
		return errors.New("must provide build command that uses {{entrypoint}}")
	}
	return nil
}

func makeLocalPath(str string) string {
	sep := string(filepath.Separator)
	return fmt.Sprintf(".%s%s%s", sep, str, sep)
}
func (r *RebuildTargets) Do(ctx context.Context) (context.Context, error) {
	rebuilds, ok := ctx.Value("rebuilds").([]string)
	if !ok {
		return nil, errors.New("no valid targets to rebuild determined")
	}

	var targetOutputs = make(map[string][]byte)

	for _, target := range rebuilds {
		cmdToRun := strings.ReplaceAll(
			r.BuildCommand,
			"{{entrypoint}}",
			makeLocalPath(target),
		)
		log.Printf("rebuilding target %q with command %q\n", target, cmdToRun)

		if r.IsDryRun {
			continue
		}

		// NOTE(ttacon): this needs to be cleaned up as it's exceedingly fragile
		// Imagine adding an extra space on accident (e.g. "go   build").
		pieces := strings.Split(cmdToRun, " ")

		// Yes, this makes an assumption.
		cmd := exec.Command(pieces[0], pieces[1:]...)
		if len(r.WorkingDirectory) > 0 {
			cmd.Dir = r.WorkingDirectory
		}

		// TODO(ttacon): add --force-all flag to not stop at first build
		// error.
		out, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		targetOutputs[target] = out
	}

	return context.WithValue(ctx, "build-logs", targetOutputs), nil
}

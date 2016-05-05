package common

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/Sirupsen/logrus"
	"gitlab.com/gitlab-org/gitlab-ci-multi-runner/helpers"
	"time"
)

type BuildState string

const (
	Pending BuildState = "pending"
	Running            = "running"
	Failed             = "failed"
	Success            = "success"
)

type Build struct {
	GetBuildResponse `yaml:",inline"`

	Trace        BuildTrace
	BuildAbort   chan os.Signal `json:"-" yaml:"-"`
	RootDir      string         `json:"-" yaml:"-"`
	BuildDir     string         `json:"-" yaml:"-"`
	CacheDir     string         `json:"-" yaml:"-"`
	Hostname     string         `json:"-" yaml:"-"`
	Runner       *RunnerConfig  `json:"runner"`
	ExecutorData ExecutorData

	// Unique ID for all running builds on this runner
	RunnerID int `json:"runner_id"`

	// Unique ID for all running builds on this runner and this project
	ProjectRunnerID int `json:"project_runner_id"`
}

func (b *Build) log() *logrus.Entry {
	return b.Runner.Log().WithField("build", b.ID)
}

func (b *Build) ProjectUniqueName() string {
	return fmt.Sprintf("runner-%s-project-%d-concurrent-%d",
		b.Runner.ShortDescription(), b.ProjectID, b.ProjectRunnerID)
}

func (b *Build) ProjectSlug() (string, error) {
	url, err := url.Parse(b.RepoURL)
	if err != nil {
		return "", err
	}
	if url.Host == "" {
		return "", errors.New("only URI reference supported")
	}

	slug := url.Path
	slug = strings.TrimSuffix(slug, ".git")
	slug = path.Clean(slug)
	if slug == "." {
		return "", errors.New("invalid path")
	}
	if strings.Contains(slug, "..") {
		return "", errors.New("it doesn't look like a valid path")
	}
	return slug, nil
}

func (b *Build) ProjectUniqueDir(sharedDir bool) string {
	dir, err := b.ProjectSlug()
	if err != nil {
		dir = fmt.Sprintf("project-%d", b.ProjectID)
	}

	// for shared dirs path is constructed like this:
	// <some-path>/runner-short-id/concurrent-id/group-name/project-name/
	// ex.<some-path>/01234567/0/group/repo/
	if sharedDir {
		dir = path.Join(
			fmt.Sprintf("%s", b.Runner.ShortDescription()),
			fmt.Sprintf("%d", b.ProjectRunnerID),
			dir,
		)
	}
	return dir
}

func (b *Build) FullProjectDir() string {
	return helpers.ToSlash(b.BuildDir)
}

func (b *Build) StartBuild(rootDir, cacheDir string, sharedDir bool) {
	b.RootDir = rootDir
	b.BuildDir = path.Join(rootDir, b.ProjectUniqueDir(sharedDir))
	b.CacheDir = path.Join(cacheDir, b.ProjectUniqueDir(false))
}

func (b *Build) executeScript(buildScript *ShellScript, executor Executor, abort chan interface{}) error {
	// Execute pre script (git clone, cache restore, artifacts download)
	err := executor.Run(ExecutorCommand{
		Script:     buildScript.PreScript,
		Predefined: true,
		Abort:      abort,
	})

	if err == nil {
		// Execute build script (user commands)
		err = executor.Run(ExecutorCommand{
			Script: buildScript.BuildScript,
			Abort:  abort,
		})

		// Execute after script (user commands)
		if buildScript.AfterScript != "" {
			timeoutCh := make(chan interface{})
			go func() {
				timeoutCh <- <-time.After(time.Minute * 5)
			}()
			executor.Run(ExecutorCommand{
				Script: buildScript.AfterScript,
				Abort:  timeoutCh,
			})
		}
	}

	// Execute post script (cache store, artifacts upload)
	if err == nil {
		err = executor.Run(ExecutorCommand{
			Script:     buildScript.PostScript,
			Predefined: true,
			Abort:      abort,
		})
	}

	return err
}

func (b *Build) run(executor Executor) (err error) {
	buildTimeout := b.Timeout
	if buildTimeout <= 0 {
		buildTimeout = DefaultTimeout
	}

	buildCanceled := make(chan bool)
	buildFinish := make(chan error)
	buildAbort := make(chan interface{})

	// Wait for cancel notification
	b.Trace.Notify(func() {
		buildCanceled <- true
	})

	// Run build script
	go func() {
		buildFinish <- b.executeScript(executor.ShellScript(), executor, buildAbort)
	}()

	// Wait for signals: cancel, timeout, abort or finish
	b.log().Debugln("Waiting for signals...")
	select {
	case <-buildCanceled:
		err = errors.New("canceled")

	case <-time.After(time.Duration(buildTimeout) * time.Second):
		err = fmt.Errorf("execution took longer than %v seconds", buildTimeout)

	case signal := <-b.BuildAbort:
		err = fmt.Errorf("aborted: %v", signal)

	case err = <-buildFinish:
		return err
	}

	b.log().Debugln("Waiting for build to finish...", err)

	// Wait till we receive that build did finish
	for {
		select {
		case buildAbort <- true:
		case <-buildFinish:
			return err
		}
	}
}

func (b *Build) Run(globalConfig *Config, trace BuildTrace) (err error) {
	defer func() {
		if err != nil {
			trace.Fail(err)
		} else {
			trace.Success()
		}
	}()
	b.Trace = trace

	executor := NewExecutor(b.Runner.Executor)
	if executor == nil {
		fmt.Fprint(trace, "Executor not found:", b.Runner.Executor)
		return errors.New("executor not found")
	}
	defer executor.Cleanup()

	err = executor.Prepare(globalConfig, b.Runner, b)
	if err == nil {
		err = b.run(executor)
	}
	executor.Finish(err)
	return err
}

func (b *Build) String() string {
	return helpers.ToYAML(b)
}

func (b *Build) GetDefaultVariables() BuildVariables {
	return BuildVariables{
		{"CI", "true", true, true, false},
		{"CI_BUILD_REF", b.Sha, true, true, false},
		{"CI_BUILD_BEFORE_SHA", b.BeforeSha, true, true, false},
		{"CI_BUILD_REF_NAME", b.RefName, true, true, false},
		{"CI_BUILD_ID", strconv.Itoa(b.ID), true, true, false},
		{"CI_BUILD_REPO", b.RepoURL, true, true, false},
		{"CI_PROJECT_ID", strconv.Itoa(b.ProjectID), true, true, false},
		{"CI_PROJECT_DIR", b.FullProjectDir(), true, true, false},
		{"CI_SERVER", "yes", true, true, false},
		{"CI_SERVER_NAME", "GitLab CI", true, true, false},
		{"CI_SERVER_VERSION", "", true, true, false},
		{"CI_SERVER_REVISION", "", true, true, false},
		{"GITLAB_CI", "true", true, true, false},
	}
}

func (b *Build) GetAllVariables() BuildVariables {
	variables := b.Runner.GetVariables()
	variables = append(variables, b.GetDefaultVariables()...)
	variables = append(variables, b.Variables...)
	return variables.Expand()
}

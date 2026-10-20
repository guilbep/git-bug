package bridgecmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/commands/completion"
	"github.com/git-bug/git-bug/commands/execenv"
	"github.com/git-bug/git-bug/util/interrupt"
)

type bridgeSyncOptions struct {
	dryRun bool
	issue  string
}

func newBridgeSyncCommand(env *execenv.Env) *cobra.Command {
	options := bridgeSyncOptions{}

	cmd := &cobra.Command{
		Use:   "sync [NAME]...",
		Short: "Sync issues two-way with every bridge that has sync enabled",
		Long: `Sync issues two-way with every bridge that has sync enabled, or the given ones.

Every bridge is pulled first, then git-bug pushes to each of them, so that an
issue, comment or change made on one forge reaches all the others in one run.

A bridge takes part when sync is enabled for it, by "git bug bridge new --sync"
or later with:

    git config git-bug.bridge.<name>.sync true

With sync, everything is pushed, whichever forge it comes from. What was
written by someone without a token on a forge is posted there with the
bridge's default-login token, under a header naming its author.

Each forge account is a git-bug identity, so the same person on two forges is
two identities, except token owners. To make someone one identity, link their
login on each bridge to it:

    git config git-bug.bridge.<name>.login-<identity-id> <login>

Run it from a single place, for example a CI job:

    git bug pull && git bug bridge sync && git bug push

git-bug's assignee:<login> labels are GitHub's assignees. To hold them as
labels on GitHub instead, for a project that wants several or people
without access, set on every clone (they would undo each other's syncs
otherwise; git-board sets it from the project's configuration):

    git config git-bug.bridge.<name>.assignees labels

After a change, "git bug bridge pull <name> --no-resume" reads every issue
once, so that the next sync moves the assignees (a --dry-run lists them).

Comments written for a pull request (line comments, replies and reviews,
anchored by their review:* metadata, as git-board writes them) go to that
pull request on GitHub, never to an issue. This command doesn't know which
pull requests are an issue's, so it brings none of their comments in:
git-board does.

The first sync of a repository mirrors every issue. --dry-run reads every
forge and lists what would be pulled and pushed, and changes nothing, locally
or on the forges.

--issue syncs one issue only: its changes on each forge it is on are
pulled, then its changes pushed to every forge, creating it where a sync
would. Nothing else is pulled or written, and the next full sync still
reads everything changed since the last one. On a forge it isn't known
on yet, the issues changed since it was created are read, to find one
that a sync from another clone created for it.

For one issue on one forge, see also "git bug bridge link" (it already is an
issue there), "git bug bridge take" (drop git-bug's changes not sent there)
and "git bug bridge unlink" (stop syncing it there).`,
		Example: `# In a repository already holding git-bug issues, get them and adopt your
# identity first, so that the bridges tag it with your logins
git bug pull origin
git bug user adopt <your-identity-id>

# One bridge per forge, with sync, each using its stored token
git bug bridge new --name gh --url https://github.com/example-owner/example-repo --sync
git bug bridge new --name gl --target gitlab --url https://gitlab.example.com/example-org/example-repo --sync

# Alice is alice on GitHub and alice-ex on GitLab: one identity, 22ea975
git config git-bug.bridge.gh.login-22ea975 alice
git config git-bug.bridge.gl.login-22ea975 alice-ex

git bug bridge sync --dry-run
git bug bridge sync
git bug push origin

# Only one issue, right now
git bug bridge sync --issue 4f2a1c9`,
		PreRunE: execenv.LoadBackend(env, execenv.EnsureUser()),
		RunE: execenv.CloseBackend(env, func(cmd *cobra.Command, args []string) error {
			return runBridgeSync(env, options, args)
		}),
		ValidArgsFunction: completion.Bridge(env),
	}

	flags := cmd.Flags()
	flags.SortFlags = false
	flags.BoolVar(&options.dryRun, "dry-run", false, "Report what would be pulled and pushed, and change nothing")
	flags.StringVar(&options.issue, "issue", "", "Sync only this issue (an id prefix)")

	return cmd
}

func runBridgeSync(env *execenv.Env, opts bridgeSyncOptions, args []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{}, 1)
	defer close(done)
	var mu sync.Mutex
	interruptCount := 0
	interrupt.RegisterCleaner(func() error {
		mu.Lock()
		if interruptCount > 0 {
			env.Err.Println("Received another interrupt before graceful stop, terminating...")
			os.Exit(0)
		}
		interruptCount++
		mu.Unlock()

		env.Err.Println("Received interrupt signal, stopping the sync...\n(Hit ctrl-c again to kill the process.)")
		cancel()
		<-done
		return nil
	})

	var results <-chan core.SyncResult
	var err error
	if opts.issue != "" {
		results, err = core.SyncIssue(ctx, env.Backend, opts.issue, args, opts.dryRun)
	} else {
		results, err = core.SyncAll(ctx, env.Backend, args, opts.dryRun)
	}
	if err != nil {
		return err
	}

	pulled, pushed, errs := 0, 0, 0
	for r := range results {
		switch {
		case r.IsError():
			errs++
		case r.Import != nil && r.Import.Event == core.ImportEventBug:
			pulled++
		case r.Export != nil && r.Export.Event == core.ExportEventBug:
			pushed++
		}
		if show(r, opts.dryRun) {
			env.Out.Println(r.String())
		}
	}

	if !opts.dryRun {
		env.Out.Printf("pulled %d issues, pushed %d issues\n", pulled, pushed)
	}
	if errs > 0 {
		return fmt.Errorf("sync finished with %d error(s)", errs)
	}
	return nil
}

// show tells whether to print a result: what happened, or in a dry run what
// would happen.
func show(r core.SyncResult, dryRun bool) bool {
	var nothing bool
	var reason string
	if r.Import != nil {
		nothing, reason = r.Import.Event == core.ImportEventNothing, r.Import.Reason
	} else {
		nothing, reason = r.Export.Event == core.ExportEventNothing, r.Export.Reason
	}
	if !nothing {
		return true
	}
	return dryRun && strings.HasPrefix(reason, "would ")
}

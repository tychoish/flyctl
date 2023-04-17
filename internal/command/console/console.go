package console

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/samber/lo"
	"github.com/spf13/cobra"

	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/client"
	"github.com/superfly/flyctl/flaps"
	"github.com/superfly/flyctl/internal/appconfig"
	"github.com/superfly/flyctl/internal/command"
	"github.com/superfly/flyctl/internal/command/ssh"
	"github.com/superfly/flyctl/internal/flag"
	"github.com/superfly/flyctl/internal/prompt"
	"github.com/superfly/flyctl/iostreams"
	"github.com/superfly/flyctl/terminal"
)

func New() *cobra.Command {
	const (
		usage = "console [machine id]"
		short = ""
		long  = "\n" // TODO
	)
	cmd := command.New(usage, short, long, runConsole, command.RequireSession, command.RequireAppName)

	cmd.Args = cobra.RangeArgs(0, 1)
	flag.Add(
		cmd,
		flag.App(),
		flag.AppConfig(),
		flag.String{
			Name:        "user",
			Shorthand:   "u",
			Description: "Unix username to connect as",
			Default:     ssh.DefaultSshUsername,
		},
		flag.Bool{
			Name:        "select",
			Shorthand:   "s",
			Description: "Select from a list of machines",
			Default:     false,
		},
	)

	return cmd
}

func runConsole(ctx context.Context) error {
	io := iostreams.FromContext(ctx)
	colorize := io.ColorScheme()
	appName := appconfig.NameFromContext(ctx)
	apiClient := client.FromContext(ctx).API()

	app, err := apiClient.GetAppCompact(ctx, appName)
	if err != nil {
		return fmt.Errorf("failed to get app: %w", err)
	}

	if app.PlatformVersion != "machines" {
		return errors.New("console is only supported for the machines platform")
	}

	flapsClient, err := flaps.New(ctx, app)
	if err != nil {
		return fmt.Errorf("failed to create flaps client: %w", err)
	}
	ctx = flaps.NewContext(ctx, flapsClient)

	appConfig := appconfig.ConfigFromContext(ctx)
	if appConfig == nil {
		appConfig, err = appconfig.FromRemoteApp(ctx, appName)
		if err != nil {
			return fmt.Errorf("failed to fetch app config from backend: %w", err)
		}
	}

	if err, extraInfo := appConfig.ValidateForMachinesPlatform(ctx); err != nil {
		fmt.Fprintln(io.ErrOut, extraInfo)
		return err
	}

	machine, ephemeral, err := selectMachine(ctx, app, appConfig)
	if err != nil {
		return err
	}

	if ephemeral {
		defer func() {
			const stopTimeout = 5 * time.Second

			stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			defer cancel()

			stopInput := api.StopMachineInput{
				ID:      machine.ID,
				Timeout: api.Duration{Duration: stopTimeout},
			}
			if err := flapsClient.Stop(stopCtx, stopInput, ""); err != nil {
				terminal.Warnf("Failed to stop ephemeral machine: %v\n", err)
				terminal.Warn("You may need to destroy it manually (`fly machine destroy`).")
				return
			}

			fmt.Fprintf(io.Out, "Waiting for ephemeral machine %s to be destroyed ...", colorize.Bold(machine.ID))
			if err := flapsClient.Wait(stopCtx, machine, api.MachineStateDestroyed, stopTimeout); err != nil {
				fmt.Fprintf(io.Out, " %s!\n", colorize.Red("failed"))
				terminal.Warnf("Failed to wait for ephemeral machine to be destroyed: %v\n", err)
				terminal.Warn("You may need to destroy it manually (`fly machine destroy`).")
			} else {
				fmt.Fprintf(io.Out, " %s.\n", colorize.Green("done"))
			}
		}()
	}

	_, dialer, err := ssh.BringUpAgent(ctx, apiClient, app, false)
	if err != nil {
		return err
	}

	params := &ssh.ConnectParams{
		Ctx:            ctx,
		Org:            app.Organization,
		Dialer:         dialer,
		Username:       flag.GetString(ctx, "user"),
		DisableSpinner: false,
	}
	sshClient, err := ssh.Connect(params, machine.PrivateIP)
	if err != nil {
		return err
	}

	return ssh.Console(ctx, sshClient, appConfig.ConsoleCommand, true)
}

func selectMachine(ctx context.Context, app *api.AppCompact, appConfig *appconfig.Config) (*api.Machine, bool, error) {
	if flag.GetBool(ctx, "select") {
		return promptForMachine(ctx, app, appConfig)
	} else if len(flag.Args(ctx)) == 1 {
		return getMachineByID(ctx)
	} else {
		return makeEphemeralMachine(ctx, app, appConfig)
	}
}

func promptForMachine(ctx context.Context, app *api.AppCompact, appConfig *appconfig.Config) (*api.Machine, bool, error) {
	if len(flag.Args(ctx)) != 0 {
		return nil, false, errors.New("machine IDs can't be used with -s/--select")
	}

	flapsClient := flaps.FromContext(ctx)
	machines, err := flapsClient.ListActive(ctx)
	if err != nil {
		return nil, false, err
	}
	machines = lo.Filter(machines, func(machine *api.Machine, _ int) bool {
		return machine.State == api.MachineStateStarted && !machine.IsFlyAppsReleaseCommand()
	})
	if len(machines) == 0 {
		return nil, false, errors.New("no machines are available")
	}

	options := []string{"create an ephemeral shared-cpu-1x machine"}
	for _, machine := range machines {
		options = append(options, fmt.Sprintf("%s: %s %s %s", machine.Region, machine.ID, machine.PrivateIP, machine.Name))
	}

	index := 0
	if err := prompt.Select(ctx, &index, "Select a machine:", "", options...); err != nil {
		return nil, false, fmt.Errorf("failed to prompt for a machine: %w", err)
	}
	if index == 0 {
		return makeEphemeralMachine(ctx, app, appConfig)
	} else {
		return machines[index-1], false, nil
	}
}

func getMachineByID(ctx context.Context) (*api.Machine, bool, error) {
	flapsClient := flaps.FromContext(ctx)
	machineID := flag.FirstArg(ctx)
	machine, err := flapsClient.Get(ctx, machineID)
	if err != nil {
		return nil, false, err
	}
	if machine.State != api.MachineStateStarted {
		return nil, false, fmt.Errorf("machine %s is not started", machineID)
	}
	if machine.IsFlyAppsReleaseCommand() {
		return nil, false, fmt.Errorf("machine %s is a release command machine", machineID)
	}

	return machine, false, nil
}

func makeEphemeralMachine(ctx context.Context, app *api.AppCompact, appConfig *appconfig.Config) (*api.Machine, bool, error) {
	io := iostreams.FromContext(ctx)
	colorize := io.ColorScheme()
	apiClient := client.FromContext(ctx).API()
	flapsClient := flaps.FromContext(ctx)

	currentRelease, err := apiClient.GetAppCurrentReleaseMachines(ctx, app.Name)
	if err != nil {
		return nil, false, err
	}
	if currentRelease == nil {
		return nil, false, errors.New("can't create an ephemeral machine since the app has not yet been released")
	}

	machConfig, err := appConfig.ToConsoleMachineConfig()
	if err != nil {
		return nil, false, fmt.Errorf("failed to generate ephemeral machine configuration: %w", err)
	}
	machConfig.Image = currentRelease.ImageRef             // TODO: double-check this
	machConfig.Guest = api.MachinePresets["shared-cpu-1x"] // TODO: infer size like with release commands?

	launchInput := api.LaunchMachineInput{
		AppID:   app.ID,
		OrgSlug: app.Organization.ID,
		Config:  machConfig,
	}
	machine, err := flapsClient.Launch(ctx, launchInput)
	if err != nil {
		return nil, false, fmt.Errorf("failed to launch ephemeral machine: %w", err)
	}
	fmt.Fprintf(io.Out, "Created an ephemeral machine %s to run the console.\n", colorize.Bold(machine.ID))

	const waitTimeout = 15 * time.Second
	fmt.Fprintf(io.Out, "Waiting for %s to start ...", colorize.Bold(machine.ID))
	err = flapsClient.Wait(ctx, machine, api.MachineStateStarted, waitTimeout)
	if err == nil {
		fmt.Fprintf(io.Out, " %s.\n", colorize.Green("done"))
		return machine, true, nil
	}

	fmt.Fprintf(io.Out, " %s!\n", colorize.Red("failed"))
	var flapsErr *flaps.FlapsError
	destroyed := false
	if errors.As(err, &flapsErr) && flapsErr.ResponseStatusCode == 404 {
		destroyed, err = checkMachineDestruction(ctx, machine, err)
	}

	if !destroyed {
		terminal.Warn("You may need to destroy the machine manually (`fly machine destroy`).")
	}
	return nil, false, err
}

func checkMachineDestruction(ctx context.Context, machine *api.Machine, firstErr error) (bool, error) {
	flapsClient := flaps.FromContext(ctx)
	machine, err := flapsClient.Get(ctx, machine.ID)
	if err != nil {
		return false, fmt.Errorf("failed to check status of machine: %w", err)
	}

	if machine.State != api.MachineStateDestroyed && machine.State != api.MachineStateDestroying {
		return false, firstErr
	}

	var exitEvent *api.MachineEvent
	for _, event := range machine.Events {
		if event.Type == "exit" {
			exitEvent = event
			break
		}
	}

	if exitEvent == nil || exitEvent.Request == nil {
		return true, errors.New("machine was destroyed unexpectedly")
	}

	exitCode, err := exitEvent.Request.GetExitCode()
	if err != nil {
		return true, errors.New("machine exited unexpectedly")
	}

	return true, fmt.Errorf("machine exited unexpectedly with code %v", exitCode)
}

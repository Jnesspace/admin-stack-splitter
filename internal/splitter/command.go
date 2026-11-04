package splitter

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
)

const commandHelp = `Split a migration-kit admin stack using local state copies.

Usage: admin-splitter <command> [flags]

Commands:
  fetch        Download the original admin's state and settings
  inspect      Show state counts or preview child groups
  prepare      Create and validate local configuration and state partitions
  check-source Check a fresh state snapshot against the prepared source
  publish      Upload a prepared split and start its parent plan in Spacelift
  verify-plan  Check a saved plan before applying it

Start with docs/quickstart.md. Publish changes remote state; plan approval remains manual.
Use admin-splitter <command> --help for options; add --json for automation.

Authentication: SPACELIFT_API_KEY_ID and SPACELIFT_API_KEY_SECRET,
or SPACELIFT_API_TOKEN. Account URL: --endpoint or SPACELIFT_API_KEY_ENDPOINT.
`

type commandError struct {
	err  error
	json bool
}

func (e *commandError) Error() string { return e.err.Error() }
func (e *commandError) Unwrap() error { return e.err }

// Execute writes command results and errors to their separate streams and returns an exit code.
func Execute(args []string, out, errOut io.Writer) int {
	if err := Run(args, out); err != nil {
		var failure *commandError
		if errors.As(err, &failure) && failure.json {
			_ = json.NewEncoder(errOut).Encode(map[string]string{"error": err.Error()})
		} else {
			_, _ = fmt.Fprintln(errOut, "Error:", err)
		}
		return 1
	}
	return 0
}

// Run executes a command, writing human output by default or JSON when requested.
func Run(args []string, out io.Writer) (err error) {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help")) {
		_, err := io.WriteString(out, commandHelp)
		return err
	}
	jsonOutput := false
	defer func() {
		if errors.Is(err, flag.ErrHelp) {
			err = nil
		} else if err != nil {
			err = &commandError{err: err, json: jsonOutput}
		}
	}()
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&jsonOutput, "json", false, "emit JSON for automation")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(out, "Usage: admin-splitter %s [flags]\n\n", args[0])
		fs.SetOutput(out)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
	}
	switch args[0] {
	case "fetch":
		stack := fs.String("stack", "", "existing admin stack ID")
		endpoint := fs.String("endpoint", os.Getenv("SPACELIFT_API_KEY_ENDPOINT"), "HTTPS Spacelift account URL")
		dir := fs.String("out", "", "new private directory for source state and settings")
		if err := parseCommand(fs, args[1:]); err != nil {
			return err
		}
		if *stack == "" || *endpoint == "" || *dir == "" {
			return errors.New("fetch requires --stack, --endpoint (or SPACELIFT_API_KEY_ENDPOINT), and --out")
		}
		if !jsonOutput {
			if _, err := fmt.Fprintln(out, "Downloading source state and settings..."); err != nil {
				return err
			}
		}
		result, err := fetchSource(context.Background(), *endpoint, *stack, *dir, credentialsFromEnvironment(), http.DefaultClient)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(out).Encode(result)
		}
		return displayFetch(out, result)
	case "inspect":
		path := fs.String("state", "", "local state snapshot")
		config := fs.String("config", "", "migration-kit configuration root; preview automatic space groups")
		if err := parseCommand(fs, args[1:]); err != nil {
			return err
		}
		if *path == "" {
			return errors.New("inspect requires --state")
		}
		s, _, err := readState(*path)
		if err != nil {
			return err
		}
		if *config != "" {
			preview, err := previewKitSplit(*config, s)
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(out).Encode(preview)
			}
			return displayPreview(out, preview)
		}
		if jsonOutput {
			return json.NewEncoder(out).Encode(s.summary())
		}
		return displayState(out, s, *path)
	case "prepare":
		config := fs.String("config", "", "generated configuration root")
		state := fs.String("state", "", "local source state snapshot")
		spec := fs.String("layout", "", "split layout JSON")
		source := fs.String("source-stack", "", "source stack settings JSON from fetch, MCP or GraphQL")
		stack := fs.String("stack", "", "existing admin stack ID; read settings using API key or token from the environment")
		endpoint := fs.String("endpoint", os.Getenv("SPACELIFT_API_KEY_ENDPOINT"), "HTTPS Spacelift account URL for --stack")
		dir := fs.String("out", "", "new output directory outside the configuration")
		tool := fs.String("tool", "tofu", "source IaC binary at the source version")
		validate := fs.Bool("validate", true, "initialize and validate all generated roots")
		if err := parseCommand(fs, args[1:]); err != nil {
			return err
		}
		if *config == "" || *state == "" || *dir == "" || (*spec == "" && *source == "" && *stack == "") {
			return errors.New("prepare requires --config, --state, --out, and --source-stack, --stack or --layout")
		}
		if *source != "" && *stack != "" {
			return errors.New("use either --source-stack or --stack, not both")
		}
		if !jsonOutput {
			message := "Preparing the local split and validating configuration..."
			if !*validate {
				message = "Preparing a diagnostic local split with validation disabled..."
			}
			if _, err := fmt.Fprintln(out, message); err != nil {
				return err
			}
		}
		var result *prepareResult
		if *source != "" || *stack != "" {
			var settings []byte
			if *source != "" {
				settings, err = os.ReadFile(*source)
			} else {
				var token string
				token, err = authenticate(context.Background(), *endpoint, credentialsFromEnvironment(), http.DefaultClient)
				if err == nil {
					settings, err = readStackSettingsWithClient(context.Background(), *endpoint, *stack, token, http.DefaultClient)
				}
			}
			if err != nil {
				return err
			}
			result, err = prepareFromStack(*config, *state, settings, *spec, *dir, *tool, *validate)
		} else {
			result, err = prepare(*config, *state, *spec, *dir, *tool, *validate)
		}
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(out).Encode(result)
		}
		return displayPreparation(out, result, *dir)
	case "check-source":
		path := fs.String("state", "", "fresh source state snapshot")
		prepared := fs.String("prepared", "", "prepare.json from this split")
		if err := parseCommand(fs, args[1:]); err != nil {
			return err
		}
		if *path == "" || *prepared == "" {
			return errors.New("check-source requires --state and --prepared")
		}
		result, err := checkSource(*path, *prepared)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(out).Encode(result)
		}
		return displaySourceCheck(out, result)
	case "publish":
		prepared := fs.String("prepared", "", "directory produced by prepare")
		endpoint := fs.String("endpoint", os.Getenv("SPACELIFT_API_KEY_ENDPOINT"), "HTTPS Spacelift account URL")
		tool := fs.String("tool", "tofu", "same OpenTofu binary used for prepare")
		confirm := fs.Bool("confirm", false, "upload configuration and states, update the parent and start a proposed run; never apply")
		if err := parseCommand(fs, args[1:]); err != nil {
			return err
		}
		if *prepared == "" || *endpoint == "" {
			return errors.New("publish requires --prepared and --endpoint (or SPACELIFT_API_KEY_ENDPOINT)")
		}
		if !jsonOutput {
			if _, err := fmt.Fprintln(out, "Checking the prepared split and current source..."); err != nil {
				return err
			}
		}
		result, err := publish(context.Background(), *endpoint, *prepared, *tool, *confirm, credentialsFromEnvironment(), http.DefaultClient)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(out).Encode(result)
		}
		if !result.Published {
			_, err = fmt.Fprintf(out, "Ready to publish: keep parent %s, create %d disabled children, upload split state and configuration to a new Spacelift repository.\nRerun with --confirm to publish and start a proposed parent plan. No apply is performed.\n", result.ParentID, len(result.Children))
			return err
		}
		_, err = fmt.Fprintf(out, "Published. Review the proposed parent plan: %s\nChildren remain disabled. Apply the parent controller changes before enabling them.\nRecovery and plan-check report: %s\n", result.RunURL, result.RecoveryDirectory)
		return err
	case "verify-plan":
		path := fs.String("plan", "", "saved plan in JSON format")
		prepared := fs.String("prepared", "", "prepare.json from this split")
		allow := fs.Bool("allow-controller-create", false, "allow only the exact controller additions listed in prepare.json")
		if err := parseCommand(fs, args[1:]); err != nil {
			return err
		}
		if *path == "" {
			return errors.New("verify-plan requires --plan")
		}
		if err := verifyPlan(*path, *prepared, *allow); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(out).Encode(map[string]bool{"valid": true})
		}
		return displayPlanCheck(out, *allow)
	default:
		return fmt.Errorf("unknown command %q; run admin-splitter --help", args[0])
	}
}

func parseCommand(fs *flag.FlagSet, args []string) error {
	usage := fs.Usage
	fs.Usage = func() {}
	err := fs.Parse(args)
	fs.Usage = usage
	if errors.Is(err, flag.ErrHelp) {
		fs.Usage()
	}
	if err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments; use named flags shown by %s --help", fs.Name())
	}
	return nil
}

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

// Run executes a command, writing its result to out.
func Run(args []string, out io.Writer) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, err := fmt.Fprintln(out, "admin-splitter fetch|inspect|prepare|verify-plan\n\nfetch: --stack ID --endpoint URL --out DIR\ninspect: --state SNAPSHOT [--config GENERATED_DIR]\nprepare: --config DIR --state SNAPSHOT --out DIR (--source-stack FILE | --stack ID --endpoint URL | --layout FILE) [--tool PATH --validate=true]\nverify-plan: --plan JSON [--prepared prepare.json --allow-controller-create]\n\nAuthentication: SPACELIFT_API_KEY_ID and SPACELIFT_API_KEY_SECRET, or SPACELIFT_API_TOKEN.\nAccount URL: --endpoint or SPACELIFT_API_KEY_ENDPOINT.")
		return err
	}
	if len(args) == 0 {
		return errors.New("usage: admin-splitter fetch|inspect|prepare|verify-plan [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(out)
	switch args[0] {
	case "fetch":
		stack := fs.String("stack", "", "existing admin stack ID")
		endpoint := fs.String("endpoint", os.Getenv("SPACELIFT_API_KEY_ENDPOINT"), "HTTPS Spacelift account URL")
		dir := fs.String("out", "", "new private directory for source state and settings")
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if *stack == "" || *endpoint == "" || *dir == "" || fs.NArg() != 0 {
			return errors.New("fetch requires --stack, --endpoint (or SPACELIFT_API_KEY_ENDPOINT), and --out")
		}
		result, err := fetchSource(context.Background(), *endpoint, *stack, *dir, credentialsFromEnvironment(), http.DefaultClient)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(result)
	case "inspect":
		path := fs.String("state", "", "local state snapshot")
		config := fs.String("config", "", "migration-kit configuration root; preview automatic space groups")
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
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
			return json.NewEncoder(out).Encode(preview)
		}
		return json.NewEncoder(out).Encode(s.summary())
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
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if *config == "" || *state == "" || *dir == "" || (*spec == "" && *source == "" && *stack == "") {
			return errors.New("prepare requires --config, --state, --out, and --source-stack, --stack or --layout")
		}
		if *source != "" && *stack != "" {
			return errors.New("use either --source-stack or --stack, not both")
		}
		var result *prepareResult
		var err error
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
		return json.NewEncoder(out).Encode(result)
	case "verify-plan":
		path := fs.String("plan", "", "saved plan in JSON format")
		prepared := fs.String("prepared", "", "prepare.json from this split")
		allow := fs.Bool("allow-controller-create", false, "allow only the exact controller additions listed in prepare.json")
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if err := verifyPlan(*path, *prepared, *allow); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]bool{"valid": true})
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

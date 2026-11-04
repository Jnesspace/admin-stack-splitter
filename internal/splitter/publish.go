package splitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
)

type publishedChild struct {
	Group   string `json:"group"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Created bool   `json:"created"`
}

type publishResult struct {
	Published         bool             `json:"published"`
	ParentID          string           `json:"parent_id"`
	Children          []publishedChild `json:"children"`
	RepositoryID      string           `json:"repository_id,omitempty"`
	Commit            string           `json:"commit,omitempty"`
	RunID             string           `json:"run_id,omitempty"`
	RunURL            string           `json:"run_url,omitempty"`
	RecoveryDirectory string           `json:"recovery_directory,omitempty"`
}

type publicationJournal struct {
	Result                 publishResult  `json:"result"`
	Endpoint               string         `json:"endpoint"`
	Pending                string         `json:"pending,omitempty"`
	Completed              []string       `json:"completed"`
	OriginalParent         map[string]any `json:"original_parent_input"`
	ParentStateObject      string         `json:"parent_state_object,omitempty"`
	FailureFreezeAttempted bool           `json:"failure_freeze_attempted,omitempty"`
	FailureFreezeSucceeded bool           `json:"failure_freeze_succeeded,omitempty"`
}

func writeJournal(file string, journal *publicationJournal) error {
	raw, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".journal-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(append(raw, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, file)
}

func publish(ctx context.Context, endpoint, root, tool string, confirm bool, credentials apiCredentials, client *http.Client) (result *publishResult, err error) {
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("prepared must be a real directory")
	}
	report, spec, err := loadPublication(root)
	if err != nil {
		return nil, err
	}
	endpoint, err = accountEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	token, err := authenticate(ctx, endpoint, credentials, client)
	if err != nil {
		return nil, err
	}
	api := publicationAPI{ctx: ctx, endpoint: endpoint, token: token, client: client}
	var viewer *struct {
		ID      string `json:"id"`
		Admin   bool   `json:"admin"`
		Actions []struct {
			SpaceID string   `json:"spaceID"`
			Actions []string `json:"actions"`
		} `json:"actions"`
	}
	if err := api.call("publication user session check", `query AdminSplitterPublisher { viewer { id admin actions { spaceID actions } } }`, nil, "viewer", &viewer); err != nil {
		return nil, err
	}
	if viewer == nil || viewer.ID == "" {
		return nil, fmt.Errorf("publish requires a user session or personal API key; machine sessions cannot import parent state")
	}
	canPublish := viewer.Admin
	for _, grant := range viewer.Actions {
		if grant.SpaceID == spec.SpaceID {
			for _, action := range grant.Actions {
				canPublish = canPublish || action == "SPACE_ADMIN"
			}
		}
	}
	if !canPublish {
		return nil, fmt.Errorf("API publication requires a user session or personal API key with Space admin access to the source space")
	}
	stack, err := api.stack(spec.SourceStackID)
	if err != nil {
		return nil, err
	}
	if stack == nil {
		return nil, fmt.Errorf("source stack was not returned")
	}
	settings, _ := json.Marshal(map[string]any{"stack": stack})
	live, err := layoutFromStackSettings(settings)
	if err != nil {
		return nil, err
	}
	if live.SourceStackID != spec.SourceStackID || live.SpaceID != spec.SpaceID || live.RoleID != spec.RoleID || live.WorkerPoolID != spec.WorkerPoolID || live.RunnerImage != spec.RunnerImage || live.Repository != spec.Repository || live.Branch != spec.Branch || live.ProjectRoot != spec.ProjectRoot || (spec.VCSProvider != "" && live.VCSProvider != spec.VCSProvider) {
		return nil, fmt.Errorf("source settings changed or differ from preparation; fetch and prepare again")
	}
	if !stackBool(stack, "managesStateFile") || stackBool(stack, "isLockedByTemplateDeployment") {
		return nil, fmt.Errorf("publish requires a standalone stack with Spacelift-managed state")
	}
	if stackString(stack, "lockedBy") != "" {
		return nil, fmt.Errorf("source is already locked; release its lock before publishing so the command can take its own lock")
	}
	input, err := parentStackInput(stack)
	if err != nil {
		return nil, err
	}
	// A generated configuration contains only .tf files and provider locks. Runtime
	// scripts and attached contexts can require files or credentials absent in children.
	var hooks map[string][]string
	_ = json.Unmarshal(stack["hooks"], &hooks)
	for _, values := range hooks {
		if len(values) != 0 {
			return nil, fmt.Errorf("API publication does not copy source hooks to children; resolve those runtime requirements before publishing")
		}
	}
	var contexts []json.RawMessage
	if json.Unmarshal(stack["attachedContexts"], &contexts) != nil || len(contexts) != 0 {
		return nil, fmt.Errorf("API publication requires a source without attached contexts; child context inheritance needs a reviewed manual migration")
	}
	if err := api.idle(spec.SourceStackID); err != nil {
		return nil, err
	}
	fresh, err := api.state(spec.SourceStackID)
	if err != nil {
		return nil, err
	}
	if checksum(fresh) != report.SourceSHA256 {
		return nil, fmt.Errorf("source state changed since preparation; fetch and prepare again")
	}
	result = &publishResult{ParentID: spec.SourceStackID}
	slugPattern := regexp.MustCompile(`[^a-z0-9-]+`)
	for _, g := range spec.Groups {
		name := g.StackName
		if name == "" {
			name = spec.SourceStackID + "-" + g.Name
		}
		slug := strings.Trim(slugPattern.ReplaceAllString(strings.ToLower(name), "-"), "-")
		if len(slug) > 180 {
			slug = slug[:180]
		}
		slug += "-" + report.SourceSHA256[:12]
		for _, other := range result.Children {
			if other.ID == slug {
				return nil, fmt.Errorf("child names produce duplicate stack IDs")
			}
		}
		child, err := api.stack(slug)
		if err != nil {
			return nil, err
		}
		if child != nil {
			return nil, fmt.Errorf("a destination child stack already exists; refusing to overwrite it")
		}
		result.Children = append(result.Children, publishedChild{Group: g.Name, ID: slug, Name: name})
	}
	work := filepath.Join(root, "private", "publish")
	if _, err := os.Lstat(work); err == nil || !os.IsNotExist(err) {
		return nil, fmt.Errorf("this preparation already has a publication record; inspect private/publish/recovery.json before doing anything else")
	}
	if !confirm {
		return result, nil
	}
	resolved, err := exec.LookPath(tool)
	if err != nil {
		return nil, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	version, err := exec.Command(resolved, "version").Output()
	if err != nil || !strings.HasPrefix(string(version), "OpenTofu v"+report.ToolVersion+"\n") {
		return nil, fmt.Errorf("publish requires the same OpenTofu version used to prepare: %s", report.ToolVersion)
	}
	if err := newDirectory(work); err != nil {
		return nil, err
	}
	result.RecoveryDirectory = work
	journal := &publicationJournal{Result: *result, Endpoint: endpoint, OriginalParent: input}
	journalFile := filepath.Join(work, "recovery.json")
	if err := writeJournal(journalFile, journal); err != nil {
		return nil, err
	}
	if err := writePrivate(filepath.Join(work, ".gitignore"), []byte("*\n")); err != nil {
		return nil, err
	}
	if err := writePrivate(filepath.Join(work, "source-stack.json"), settings); err != nil {
		return nil, err
	}
	if err := writePrivate(filepath.Join(work, "original.tfstate"), fresh); err != nil {
		return nil, err
	}
	// Validate the exact publication shape before touching the account. Replacing
	// a literal repository ID later cannot change HCL structure or dependencies.
	snapshot := filepath.Join(work, "snapshot")
	if err := snapshotPublicationConfig(root, snapshot, report); err != nil {
		return nil, err
	}
	config := filepath.Join(work, "repo")
	review, err := publicationConfig(snapshot, config, spec, report, "publication-repository", result.Children)
	if err != nil {
		return nil, err
	}
	log := filepath.Join(work, "validate.log")
	for _, args := range [][]string{{"init", "-backend=false", "-input=false", "-no-color"}, {"validate", "-no-color"}} {
		if err := localTool(resolved, config, log, args...); err != nil {
			return nil, err
		}
	}
	mutated := false
	defer func() {
		if err == nil || !mutated {
			return
		}
		// Never try to roll back ownership automatically after an uncertain response.
		// Pause the parent again even if the failure happened after releasing it.
		freezeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		freezeAPI := api
		freezeAPI.ctx = freezeCtx
		journal.FailureFreezeAttempted = true
		journal.FailureFreezeSucceeded = freezeAPI.stackAction("stackDisable", spec.SourceStackID, "") == nil
		_ = writeJournal(journalFile, journal)
		err = fmt.Errorf("%w; publication stopped, inspect %s (parent pause confirmed: %t); do not rerun or enable the children", err, journalFile, journal.FailureFreezeSucceeded)
	}()
	step := func(name string, action func() error) error {
		journal.Pending = name
		journal.Result = *result
		if err := writeJournal(journalFile, journal); err != nil {
			return err
		}
		mutated = true
		if err := action(); err != nil {
			return err
		}
		journal.Completed = append(journal.Completed, name)
		journal.Pending = ""
		journal.Result = *result
		return writeJournal(journalFile, journal)
	}
	if err = step("disable parent", func() error { return api.stackAction("stackDisable", spec.SourceStackID, "") }); err != nil {
		return nil, err
	}
	if err = step("lock parent", func() error {
		return api.stackAction("stackLock", spec.SourceStackID, "admin-splitter publication "+report.SourceSHA256[:12])
	}); err != nil {
		return nil, err
	}
	if err = api.idle(spec.SourceStackID); err != nil {
		return nil, err
	}
	fresh, err = api.state(spec.SourceStackID)
	if err != nil {
		return nil, err
	}
	if checksum(fresh) != report.SourceSHA256 {
		return nil, fmt.Errorf("source state changed while taking the lock")
	}
	frozen, err := api.stack(spec.SourceStackID)
	if err != nil {
		return nil, err
	}
	if !stackBool(frozen, "isDisabled") || stackString(frozen, "lockedBy") == "" || stackString(frozen, "lockNote") != "admin-splitter publication "+report.SourceSHA256[:12] {
		return nil, fmt.Errorf("parent pause and publication lock were not confirmed")
	}
	frozenInput, err := parentStackInput(frozen)
	if err != nil {
		return nil, err
	}
	frozenInput["isDisabled"] = input["isDisabled"]
	if !reflect.DeepEqual(frozenInput, input) {
		return nil, fmt.Errorf("source settings changed while taking the lock")
	}
	if err = step("create managed repository", func() error {
		var repo struct {
			ID string `json:"id"`
		}
		vars := map[string]any{"input": map[string]any{"name": "admin-split-" + report.SourceSHA256[:12], "spaceID": spec.SpaceID, "labels": []string{"admin-splitter"}, "isDefault": false}}
		if err := api.call("repository creation", `mutation AdminSplitterRepoCreate($input: RepoCreateInput!) { repoCreate(input:$input) { id } }`, vars, "repoCreate", &repo); err != nil {
			return err
		}
		if repo.ID == "" {
			return fmt.Errorf("repository creation returned no ID")
		}
		result.RepositoryID = repo.ID
		return nil
	}); err != nil {
		return nil, err
	}
	// Replace the validation-only copy with the final literal repository ID.
	finalConfig := filepath.Join(work, "published-repo")
	review, err = publicationConfig(snapshot, finalConfig, spec, report, result.RepositoryID, result.Children)
	if err != nil {
		return nil, err
	}
	if err = writeJSON(filepath.Join(work, "review.json"), review); err != nil {
		return nil, err
	}
	files, err := revisionFiles(finalConfig, spec.ProjectRoot)
	if err != nil {
		return nil, err
	}
	if err = step("commit split configuration", func() error {
		var revision struct {
			SHA string `json:"sha"`
		}
		vars := map[string]any{"input": map[string]any{"repoID": result.RepositoryID, "message": "Split admin configuration", "authorName": "Platform administrator", "files": files}}
		if err := api.call("configuration publication", `mutation AdminSplitterRevision($input: RevisionCreateInput!) { revisionCreate(input:$input) { sha } }`, vars, "revisionCreate", &revision); err != nil {
			return err
		}
		if revision.SHA == "" {
			return fmt.Errorf("configuration publication returned no commit")
		}
		result.Commit = revision.SHA
		return nil
	}); err != nil {
		return nil, err
	}
	for i := range result.Children {
		child := &result.Children[i]
		if err = step("create child "+child.Group, func() error {
			raw, err := readArtifact(root, report.Groups[i].StateFile, maxStateDownloadBytes)
			if err != nil {
				return err
			}
			if checksum(raw) != report.Artifacts[report.Groups[i].StateFile] {
				return fmt.Errorf("child state changed after preflight")
			}
			object, err := api.upload(raw)
			if err != nil {
				return err
			}
			childInput := map[string]any{
				"name": child.Name, "space": spec.SpaceID, "repository": result.RepositoryID, "vcsIntegrationId": result.RepositoryID,
				"provider": "SPACELIFT", "branch": "main", "projectRoot": path.Join(spec.ProjectRoot, "_split", child.Group),
				"autodeploy": false, "isDisabled": true, "labels": []string{"admin-split-child"},
				"githubActionDeploy": true, "enableSensitiveOutputUpload": true,
				"vendorConfig": map[string]any{"terraform": map[string]any{"version": spec.ToolVersion, "useSmartSanitization": true, "externalStateAccessEnabled": false, "workflowTool": "OPEN_TOFU"}},
			}
			if spec.WorkerPoolID != "" {
				childInput["workerPool"] = spec.WorkerPoolID
			}
			if spec.RunnerImage != "" {
				childInput["runnerImage"] = spec.RunnerImage
			}
			var created struct {
				ID       string `json:"id"`
				Disabled bool   `json:"isDisabled"`
				Pool     *struct {
					ID string `json:"id"`
				} `json:"workerPool"`
				RunnerImage string `json:"runnerImage"`
			}
			vars := map[string]any{"input": childInput, "slug": child.ID, "state": object}
			if err := api.call("child creation with state", `mutation AdminSplitterChildCreate($input: StackInput!, $slug: String!, $state: String!) { stackCreate(input:$input, slug:$slug, manageState:true, stackObjectID:$state) { id isDisabled workerPool { id } runnerImage } }`, vars, "stackCreate", &created); err != nil {
				return err
			}
			if created.ID != child.ID || !created.Disabled {
				return fmt.Errorf("child creation did not return the expected disabled stack")
			}
			pool := ""
			if created.Pool != nil {
				pool = created.Pool.ID
			}
			if pool != spec.WorkerPoolID || created.RunnerImage != spec.RunnerImage {
				return fmt.Errorf("child runtime differs from the source; leaving it disabled")
			}
			child.Created = true
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if err = step("import reduced parent state", func() error {
		current, err := api.state(spec.SourceStackID)
		if err != nil {
			return err
		}
		if checksum(current) != report.SourceSHA256 {
			return fmt.Errorf("source state changed before import; refusing to overwrite it")
		}
		raw, err := readArtifact(root, "private/states/parent.tfstate", maxStateDownloadBytes)
		if err != nil {
			return err
		}
		if checksum(raw) != report.Artifacts["private/states/parent.tfstate"] {
			return fmt.Errorf("parent partition changed after preflight")
		}
		object, err := api.upload(raw)
		if err != nil {
			return err
		}
		journal.ParentStateObject = object
		if err := writeJournal(journalFile, journal); err != nil {
			return err
		}
		// This nullable Boolean mutation can return null on success.
		if err := api.call("parent state import (requires a user session or personal API key)", `mutation AdminSplitterParentImport($id: ID!, $state: String!) { stackManagedStateImport(stackId:$id,state:$state) }`, map[string]string{"id": spec.SourceStackID, "state": object}, "stackManagedStateImport", nil); err != nil {
			return err
		}
		current, err = api.state(spec.SourceStackID)
		if err != nil {
			return err
		}
		if checksum(current) != checksum(raw) {
			return fmt.Errorf("imported parent state does not match the prepared partition")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err = step("point parent at split repository", func() error {
		updated := map[string]any{}
		inputJSON, _ := json.Marshal(input)
		_ = json.Unmarshal(inputJSON, &updated)
		updated["provider"], updated["repository"], updated["vcsIntegrationId"] = "SPACELIFT", result.RepositoryID, result.RepositoryID
		updated["branch"], updated["namespace"], updated["repositoryURL"] = "main", "", nil
		updated["autodeploy"], updated["isDisabled"] = false, true
		// The new repository contains only this split. Old sparse paths can omit
		// _split/, preventing the parent from hashing child configuration changes.
		updated["gitSparseCheckoutPaths"] = nil
		for _, vendor := range updated["vendorConfig"].(map[string]any) {
			vendor.(map[string]any)["version"] = spec.ToolVersion
		}
		var saved struct {
			ID string `json:"id"`
		}
		if err := api.call("parent repository update", `mutation AdminSplitterParentUpdate($id: ID!, $input: StackInput!) { stackUpdate(id:$id,input:$input) { id } }`, map[string]any{"id": spec.SourceStackID, "input": updated}, "stackUpdate", &saved); err != nil {
			return err
		}
		if saved.ID != spec.SourceStackID {
			return fmt.Errorf("parent update returned an unexpected stack")
		}
		current, err := api.stack(spec.SourceStackID)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(map[string]any{"stack": current})
		actual, err := layoutFromStackSettings(raw)
		if err != nil {
			return err
		}
		if actual.WorkerPoolID != spec.WorkerPoolID || actual.RunnerImage != spec.RunnerImage || actual.Repository != result.RepositoryID || actual.Branch != "main" || actual.ProjectRoot != spec.ProjectRoot || actual.RoleID != spec.RoleID || !stackBool(current, "isDisabled") || stackBool(current, "autodeploy") {
			return fmt.Errorf("parent settings differ from the requested publication; leaving it paused")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err = step("set parent current commit", func() error {
		var saved struct {
			ID     string `json:"id"`
			Commit struct {
				Hash string `json:"hash"`
			} `json:"trackedCommit"`
		}
		if err := api.call("parent current commit", `mutation AdminSplitterCurrentCommit($id: ID!, $sha: String!) { stackSetCurrentCommit(id:$id,sha:$sha) { id trackedCommit { hash } } }`, map[string]string{"id": spec.SourceStackID, "sha": result.Commit}, "stackSetCurrentCommit", &saved); err != nil {
			return err
		}
		if saved.ID != spec.SourceStackID || saved.Commit.Hash != result.Commit {
			return fmt.Errorf("parent current commit does not match the published configuration")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err = step("enable parent for review", func() error { return api.stackAction("stackEnable", spec.SourceStackID, "") }); err != nil {
		return nil, err
	}
	// Keep our lock until the proposed run has been accepted. No apply API is called.
	if err = step("start proposed parent plan", func() error {
		var run struct {
			ID string `json:"id"`
		}
		if err := api.call("proposed parent run", `mutation AdminSplitterReviewRun($stack: ID!, $commit: String!) { runTrigger(stack:$stack,commitSha:$commit,runType:PROPOSED) { id } }`, map[string]string{"stack": spec.SourceStackID, "commit": result.Commit}, "runTrigger", &run); err != nil {
			return err
		}
		if run.ID == "" {
			return fmt.Errorf("proposed run returned no ID")
		}
		result.RunID = run.ID
		u, _ := url.Parse(endpoint)
		u.Path = "/stack/" + url.PathEscape(spec.SourceStackID) + "/run/" + url.PathEscape(run.ID)
		result.RunURL = u.String()
		return nil
	}); err != nil {
		return nil, err
	}
	if err = step("unlock parent", func() error { return api.stackAction("stackUnlock", spec.SourceStackID, "") }); err != nil {
		return nil, err
	}
	result.Published = true
	journal.Result = *result
	if err = writeJournal(journalFile, journal); err != nil {
		return nil, err
	}
	return result, nil
}

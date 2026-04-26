package bd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/awell-health/spire/pkg/config"
)

// --- Option structs ---

// CreateOpts are options for creating a bead.
type CreateOpts struct {
	Type        string // task, bug, feature, epic, chore
	Priority    *int   // 0-4; nil omits the flag
	Parent      string // parent bead ID
	Description string
}

// ListOpts are options for listing beads.
type ListOpts struct {
	Status string // filter by status
	Type   string // filter by type
	Label  string // filter by label
	Rig    string // filter by rig (database)
}

// UpdateOpts are options for updating a bead.
type UpdateOpts struct {
	Claim       *bool  // set --claim
	Status      string // set --status
	Title       string // set --title
	Description string // set --description
	Owner       string // set --owner
	AddLabel    string // set --add-label
	RemoveLabel string // set --remove-label
}

// InitOpts are options for initializing beads.
//
// Server mode (Server=true) is required for CGO-disabled builds
// because bd's embedded Dolt engine needs CGO. When Server is set,
// bd connects to an external dolt sql-server using ServerHost/Port/User;
// the server password is read from BEADS_DOLT_PASSWORD by bd itself.
type InitOpts struct {
	Database   string
	Prefix     string
	Force      bool
	Server     bool   // pass --server (external dolt sql-server)
	ServerHost string // --server-host; empty omits
	ServerPort int    // --server-port; 0 omits
	ServerUser string // --server-user; empty omits
}

// --- Arg builders (exported for testability within package) ---

func buildCreateArgs(title string, opts CreateOpts) []string {
	args := []string{"create", title}
	if opts.Type != "" {
		args = append(args, "-t", opts.Type)
	}
	if opts.Priority != nil {
		args = append(args, "-p", strconv.Itoa(*opts.Priority))
	}
	if opts.Parent != "" {
		args = append(args, "--parent", opts.Parent)
	}
	if opts.Description != "" {
		args = append(args, "--description", opts.Description)
	}
	return args
}

func buildListArgs(opts ListOpts) []string {
	args := []string{"list"}
	if opts.Status != "" {
		args = append(args, "--status="+opts.Status)
	}
	if opts.Type != "" {
		args = append(args, "--type", opts.Type)
	}
	if opts.Label != "" {
		args = append(args, "--label", opts.Label)
	}
	if opts.Rig != "" {
		args = append(args, "--prefix="+opts.Rig)
	}
	return args
}

func buildUpdateArgs(id string, opts UpdateOpts) []string {
	args := []string{"update", id}
	if opts.Claim != nil && *opts.Claim {
		args = append(args, "--claim")
	}
	if opts.Status != "" {
		args = append(args, "--status", opts.Status)
	}
	if opts.Title != "" {
		args = append(args, "--title", opts.Title)
	}
	if opts.Description != "" {
		args = append(args, "--description", opts.Description)
	}
	if opts.Owner != "" {
		args = append(args, "--owner", opts.Owner)
	}
	if opts.AddLabel != "" {
		args = append(args, "--add-label", opts.AddLabel)
	}
	if opts.RemoveLabel != "" {
		args = append(args, "--remove-label", opts.RemoveLabel)
	}
	return args
}

func buildMolArgs(subcmd, formula string, vars map[string]string) []string {
	args := []string{"mol", subcmd, formula}
	for k, v := range vars {
		args = append(args, "--var", k+"="+v)
	}
	return args
}

func buildInitArgs(opts InitOpts) []string {
	args := []string{"init"}
	if opts.Database != "" {
		args = append(args, "--database", opts.Database)
	}
	if opts.Prefix != "" {
		args = append(args, "--prefix", opts.Prefix)
	}
	if opts.Server {
		args = append(args, "--server")
	}
	if opts.ServerHost != "" {
		args = append(args, "--server-host", opts.ServerHost)
	}
	if opts.ServerPort != 0 {
		args = append(args, "--server-port", strconv.Itoa(opts.ServerPort))
	}
	if opts.ServerUser != "" {
		args = append(args, "--server-user", opts.ServerUser)
	}
	if opts.Force {
		args = append(args, "--force")
	}
	return args
}

// --- Command methods ---

// Create creates a new bead and returns its ID.
func (c *Client) Create(title string, opts CreateOpts) (string, error) {
	return c.execSilent(buildCreateArgs(title, opts)...)
}

// List returns beads matching the filter options.
func (c *Client) List(opts ListOpts) ([]Bead, error) {
	var beads []Bead
	if err := c.execJSON(&beads, buildListArgs(opts)...); err != nil {
		return nil, err
	}
	return beads, nil
}

// Show returns a single bead by ID.
func (c *Client) Show(id string) (*Bead, error) {
	out, err := c.exec("show", id, "--json")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, fmt.Errorf("bd show %s: empty response", id)
	}
	var bead Bead
	if err := parseBeadJSON(out, &bead); err != nil {
		return nil, fmt.Errorf("bd show %s: %w", id, err)
	}
	return &bead, nil
}

// Update updates a bead's fields.
func (c *Client) Update(id string, opts UpdateOpts) error {
	_, err := c.exec(buildUpdateArgs(id, opts)...)
	return err
}

// Close closes a bead.
func (c *Client) Close(id string) error {
	_, err := c.exec("close", id)
	return err
}

// Ready returns beads with no open blockers.
func (c *Client) Ready() ([]Bead, error) {
	var beads []Bead
	if err := c.execJSON(&beads, "ready"); err != nil {
		return nil, err
	}
	return beads, nil
}

// Count returns the number of beads.
func (c *Client) Count() (int, error) {
	out, err := c.exec("count")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// Status checks the database connection. Returns nil on success.
func (c *Client) Status() error {
	_, err := c.exec("status")
	return err
}

// ConfigGet retrieves a config value by key.
// Returns ("", nil) when the key is not set. The bd CLI prints
// "key (not set)" with exit 0 for missing keys — this method
// detects that sentinel and normalizes to an empty string.
func (c *Client) ConfigGet(key string) (string, error) {
	val, err := c.exec("config", "get", key)
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(val, "(not set)") {
		return "", nil
	}
	return val, nil
}

// ConfigSet sets a config key to value.
func (c *Client) ConfigSet(key, value string) error {
	_, err := c.exec("config", "set", key, value)
	return err
}

// ConfigUnset removes a config key.
func (c *Client) ConfigUnset(key string) error {
	_, err := c.exec("config", "unset", key)
	return err
}

// CommentAdd adds a comment to a bead.
func (c *Client) CommentAdd(id, text string) error {
	_, err := c.exec("comments", "add", id, text)
	return err
}

// DepAdd adds a dependency: blocker blocks blocked.
func (c *Client) DepAdd(blocked, blocker string) error {
	_, err := c.exec("dep", "add", blocked, blocker)
	return err
}

// Children returns the children of a bead.
func (c *Client) Children(id string) ([]Bead, error) {
	var beads []Bead
	if err := c.execJSON(&beads, "children", id); err != nil {
		return nil, err
	}
	return beads, nil
}

// Export exports all beads as JSON.
func (c *Client) Export() ([]Bead, error) {
	var beads []Bead
	if err := c.execJSON(&beads, "export"); err != nil {
		return nil, err
	}
	return beads, nil
}

// MolPour pours a molecule (workflow template).
func (c *Client) MolPour(formula string, vars map[string]string) (string, error) {
	return c.exec(buildMolArgs("pour", formula, vars)...)
}

// MolCook cooks a molecule (check/prepare without pouring).
func (c *Client) MolCook(formula string, vars map[string]string) (string, error) {
	return c.exec(buildMolArgs("cook", formula, vars)...)
}

// DoltCommit commits changes in the dolt database.
//
// gateway-mode: rejected with ErrGatewayDirectMutation. Dolt commits live
// inside the cluster for cluster-as-truth deployments; a laptop fronting
// a gateway-mode tower must not stamp local commits onto a database the
// cluster owns. Read-only wrappers (DoltSQL queries, DoltRemoteList) are
// intentionally not gated.
func (c *Client) DoltCommit(message string) error {
	if err := config.EnsureNotGatewayResolved("bd.DoltCommit"); err != nil {
		return err
	}
	_, err := c.exec("dolt", "commit", message)
	return err
}

// DoltPush pushes to a dolt remote. Empty remote/branch uses defaults.
//
// gateway-mode: rejected with ErrGatewayDirectMutation. Pushing the
// laptop's local Dolt to the canonical remote bypasses the gateway and
// recreates the divergence class cluster-as-truth eliminates.
func (c *Client) DoltPush(remote, branch string) error {
	if err := config.EnsureNotGatewayResolved("bd.DoltPush"); err != nil {
		return err
	}
	args := []string{"dolt", "push"}
	if remote != "" {
		args = append(args, remote)
	}
	if branch != "" {
		args = append(args, branch)
	}
	_, err := c.exec(args...)
	return err
}

// DoltPull pulls from a dolt remote. Empty remote/branch uses defaults.
//
// gateway-mode: rejected with ErrGatewayDirectMutation. Pulling mutates
// the laptop's local Dolt branch state; in cluster-as-truth the laptop
// has no local Dolt to mutate, and even when it does the cluster's
// authoritative graph should be reached through the gateway.
func (c *Client) DoltPull(remote, branch string) error {
	if err := config.EnsureNotGatewayResolved("bd.DoltPull"); err != nil {
		return err
	}
	args := []string{"dolt", "pull"}
	if remote != "" {
		args = append(args, remote)
	}
	if branch != "" {
		args = append(args, branch)
	}
	_, err := c.exec(args...)
	return err
}

// DoltRemoteAdd adds a dolt remote.
//
// gateway-mode: rejected with ErrGatewayDirectMutation. Configuring a
// remote on a gateway-mode tower stages credentials/state for a direct
// push that the gateway-aware guards reject; refuse here to avoid
// leaving misleading remote state on disk.
func (c *Client) DoltRemoteAdd(name, url string) error {
	if err := config.EnsureNotGatewayResolved("bd.DoltRemoteAdd"); err != nil {
		return err
	}
	_, err := c.exec("dolt", "remote", "add", name, url)
	return err
}

// DoltRemoteRemove removes a dolt remote.
//
// gateway-mode: rejected with ErrGatewayDirectMutation. Same rationale as
// DoltRemoteAdd — remote management on a gateway-mode tower is a no-op at
// best and a source of confusing state at worst.
func (c *Client) DoltRemoteRemove(name string) error {
	if err := config.EnsureNotGatewayResolved("bd.DoltRemoteRemove"); err != nil {
		return err
	}
	_, err := c.exec("dolt", "remote", "remove", name)
	return err
}

// DoltRemoteList lists dolt remotes. Returns raw output (not structured JSON).
func (c *Client) DoltRemoteList() (string, error) {
	return c.exec("dolt", "remote", "list")
}

// DoltSQL runs a SQL query against the dolt database.
func (c *Client) DoltSQL(query string) (string, error) {
	return c.exec("dolt", "sql", "-q", query)
}

// Init initializes a beads database.
func (c *Client) Init(opts InitOpts) error {
	_, err := c.exec(buildInitArgs(opts)...)
	return err
}

// --- Package-level convenience functions using DefaultClient ---

// Create creates a new bead using the default client.
func Create(title string, opts CreateOpts) (string, error) {
	return defaultClient.Create(title, opts)
}

// List returns beads using the default client.
func List(opts ListOpts) ([]Bead, error) {
	return defaultClient.List(opts)
}

// Show returns a bead by ID using the default client.
func Show(id string) (*Bead, error) {
	return defaultClient.Show(id)
}

// Update updates a bead using the default client.
func Update(id string, opts UpdateOpts) error {
	return defaultClient.Update(id, opts)
}

// Close closes a bead using the default client.
func Close(id string) error {
	return defaultClient.Close(id)
}

// Ready returns ready beads using the default client.
func Ready() ([]Bead, error) {
	return defaultClient.Ready()
}

// Count returns the bead count using the default client.
func Count() (int, error) {
	return defaultClient.Count()
}

// Status checks connectivity using the default client.
func Status() error {
	return defaultClient.Status()
}

// ConfigGet retrieves a config value using the default client.
func ConfigGet(key string) (string, error) {
	return defaultClient.ConfigGet(key)
}

// ConfigSet sets a config value using the default client.
func ConfigSet(key, value string) error {
	return defaultClient.ConfigSet(key, value)
}

// ConfigUnset removes a config key using the default client.
func ConfigUnset(key string) error {
	return defaultClient.ConfigUnset(key)
}

// CommentAdd adds a comment using the default client.
func CommentAdd(id, text string) error {
	return defaultClient.CommentAdd(id, text)
}

// DepAdd adds a dependency using the default client.
func DepAdd(blocked, blocker string) error {
	return defaultClient.DepAdd(blocked, blocker)
}

// Children returns children using the default client.
func Children(id string) ([]Bead, error) {
	return defaultClient.Children(id)
}

// MolPour pours a molecule using the default client.
func MolPour(formula string, vars map[string]string) (string, error) {
	return defaultClient.MolPour(formula, vars)
}

// MolCook cooks a molecule using the default client.
func MolCook(formula string, vars map[string]string) (string, error) {
	return defaultClient.MolCook(formula, vars)
}

// DoltCommit commits using the default client.
func DoltCommit(message string) error {
	return defaultClient.DoltCommit(message)
}

// DoltPush pushes using the default client.
func DoltPush(remote, branch string) error {
	return defaultClient.DoltPush(remote, branch)
}

// DoltPull pulls using the default client.
func DoltPull(remote, branch string) error {
	return defaultClient.DoltPull(remote, branch)
}

// DoltRemoteAdd adds a remote using the default client.
func DoltRemoteAdd(name, url string) error {
	return defaultClient.DoltRemoteAdd(name, url)
}

// DoltRemoteList lists remotes using the default client.
func DoltRemoteList() (string, error) {
	return defaultClient.DoltRemoteList()
}

// DoltSQL runs SQL using the default client.
func DoltSQL(query string) (string, error) {
	return defaultClient.DoltSQL(query)
}

// Init initializes beads using the default client.
func Init(opts InitOpts) error {
	return defaultClient.Init(opts)
}

// --- Internal helpers ---

// parseBeadJSON handles bd show output, which may be a single object or an array.
func parseBeadJSON(raw string, bead *Bead) error {
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 {
		return fmt.Errorf("empty JSON")
	}
	// bd show --json sometimes returns an array with one element
	if raw[0] == '[' {
		var beads []Bead
		if err := json.Unmarshal([]byte(raw), &beads); err != nil {
			return err
		}
		if len(beads) == 0 {
			return fmt.Errorf("empty array")
		}
		*bead = beads[0]
		return nil
	}
	return json.Unmarshal([]byte(raw), bead)
}

// Package node runs TSUNAMI as a self-contained XBoard-managed backend.
package node

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/control"
	"github.com/tsunami-protocol/tsunami/pkg/control/adapters/xboardtsunami"
	"github.com/tsunami-protocol/tsunami/pkg/server"
)

// Config contains only the bootstrap information needed to reach XBoard.
// Listener, TLS, fronting, and protocol settings are owned by the remote
// TSUNAMI node configuration and are never duplicated in a local node file.
type Config struct {
	XBoard xboardtsunami.Config
}

type runningServer struct {
	server *server.Server
	done   chan struct{}
}

// Node owns the data plane, dynamic user store, and XBoard synchronization
// lifecycle for one TSUNAMI node.
type Node struct {
	adapter *xboardtsunami.Adapter
	users   *control.UserStore
	usage   *control.UsageTracker
	traffic control.TrafficPolicy

	mu              sync.Mutex
	cancel          context.CancelFunc
	closed          bool
	started         bool
	runtime         *runningServer
	configRevision  string
	pendingReportID string
	pendingDeltas   []control.UsageDelta

	dataPlaneMu sync.Mutex
	reportMu    sync.Mutex
	runWG       sync.WaitGroup
	closeOnce   sync.Once
	done        chan struct{}
	fatal       chan error
}

// New creates a panel-managed TSUNAMI node without enrolling or opening a
// listener. Enrollment happens in Start so configuration failures are visible
// to the service manager as startup failures.
func New(config Config) (*Node, error) {
	adapter, err := xboardtsunami.New(config.XBoard)
	if err != nil {
		return nil, err
	}

	users := control.NewEmptyUserStore()
	usage := control.NewUsageTracker()
	return &Node{
		adapter: adapter,
		users:   users,
		usage:   usage,
		traffic: control.TrafficPolicy{
			Limiter: control.NewUserLimiter(),
			Usage:   usage,
		},
		done:  make(chan struct{}),
		fatal: make(chan error, 1),
	}, nil
}

// Start enrolls if needed, obtains the initial remote config and user
// snapshot, then serves until the context is cancelled, Close is called, or
// the active data plane cannot start.
func (n *Node) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := n.markStarted(); err != nil {
		return err
	}

	if err := n.adapter.EnsureEnrolled(ctx); err != nil {
		return fmt.Errorf("tsunami node: enroll: %w", err)
	}
	remote, unchanged, err := n.adapter.FetchConfig(ctx)
	if err != nil {
		return fmt.Errorf("tsunami node: initial XBoard config: %w", err)
	}
	if unchanged || remote == nil {
		return errors.New("tsunami node: initial XBoard config was unexpectedly unchanged")
	}
	if err := n.SyncUsers(ctx); err != nil {
		return fmt.Errorf("tsunami node: initial XBoard users: %w", err)
	}
	if err := n.applyRemoteConfig(remote); err != nil {
		return fmt.Errorf("tsunami node: start data plane: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		cancel()
		return nil
	}
	n.cancel = cancel
	n.mu.Unlock()

	n.runWG.Add(1)
	go func() {
		defer n.runWG.Done()
		n.run(runCtx)
	}()

	select {
	case <-ctx.Done():
		return n.Close()
	case <-n.done:
		return nil
	case err := <-n.fatal:
		closeErr := n.Close()
		return errors.Join(fmt.Errorf("tsunami node: data plane stopped: %w", err), closeErr)
	}
}

func (n *Node) markStarted() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return errors.New("tsunami node: already closed")
	}
	if n.started {
		return errors.New("tsunami node: already started")
	}
	n.started = true
	return nil
}

// Sync fetches and atomically applies the current XBoard user snapshot.
func (n *Node) Sync(ctx context.Context) error {
	return n.SyncUsers(ctx)
}

// SyncUsers applies only fresh snapshots. HTTP 304 intentionally keeps the
// current store unchanged, while HTTP 200 with an empty list clears it.
func (n *Node) SyncUsers(ctx context.Context) error {
	snapshot, unchanged, err := n.adapter.FetchSnapshot(ctx)
	if err != nil {
		return err
	}
	if unchanged {
		return nil
	}
	if snapshot == nil {
		return errors.New("tsunami node: XBoard returned an empty user snapshot")
	}
	if err := control.ApplyMiddleware(ctx, snapshot, control.NormalizeUsers(), control.ValidateUsers()); err != nil {
		return err
	}
	if err := n.users.ApplySnapshot(snapshot); err != nil {
		return err
	}
	log.Printf("tsunami node: applied XBoard user snapshot version=%q users=%d", snapshot.Version, len(snapshot.Users))
	return nil
}

// SyncConfig checks the remote listener configuration and replaces the data
// plane only when its revision changes.
func (n *Node) SyncConfig(ctx context.Context) error {
	remote, unchanged, err := n.adapter.FetchConfig(ctx)
	if err != nil {
		return err
	}
	if unchanged || remote == nil {
		return nil
	}
	return n.applyRemoteConfig(remote)
}

// SyncNow performs one immediate control-plane pass. It is used by the
// service manager's sync command and deliberately attempts every operation
// so a transient failure in one endpoint does not suppress the others.
func (n *Node) SyncNow(ctx context.Context) error {
	return errors.Join(
		n.SyncConfig(ctx),
		n.SyncUsers(ctx),
		n.ReportUsage(ctx),
	)
}

func (n *Node) applyRemoteConfig(remote *xboardtsunami.RemoteConfig) error {
	if remote == nil {
		return errors.New("tsunami node: nil remote config")
	}
	n.mu.Lock()
	currentRevision := n.configRevision
	closed := n.closed
	n.mu.Unlock()
	if closed {
		return nil
	}
	if remote.Revision == currentRevision {
		return nil
	}

	serverConfig, err := remote.ToServerConfig(n.users, n.traffic, n.adapter)
	if err != nil {
		return err
	}
	instance, err := server.New(serverConfig)
	if err != nil {
		return err
	}

	n.dataPlaneMu.Lock()
	defer n.dataPlaneMu.Unlock()

	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	if remote.Revision == n.configRevision {
		n.mu.Unlock()
		return nil
	}
	previous := n.runtime
	n.mu.Unlock()

	if err := stopRuntime(previous); err != nil {
		return fmt.Errorf("stop previous data plane: %w", err)
	}

	runtime := &runningServer{server: instance, done: make(chan struct{})}
	n.mu.Lock()
	n.runtime = runtime
	n.configRevision = remote.Revision
	n.mu.Unlock()
	n.startRuntime(runtime)
	log.Printf("tsunami node: applied XBoard config revision=%q listen=%s", remote.Revision, remote.Listen)
	return nil
}

func (n *Node) startRuntime(runtime *runningServer) {
	go func() {
		err := runtime.server.Start()
		close(runtime.done)
		if err == nil {
			return
		}

		n.mu.Lock()
		active := n.runtime == runtime && !n.closed
		n.mu.Unlock()
		if active {
			select {
			case n.fatal <- err:
			default:
			}
		}
	}()
}

func stopRuntime(runtime *runningServer) error {
	if runtime == nil {
		return nil
	}
	err := runtime.server.Close()
	<-runtime.done
	return err
}

// ReportUsage submits accumulated traffic. A failed request leaves both its
// report ID and deltas pending so retries are idempotent at the panel.
func (n *Node) ReportUsage(ctx context.Context) error {
	n.reportMu.Lock()
	defer n.reportMu.Unlock()

	n.mu.Lock()
	reportID := n.pendingReportID
	deltas := append([]control.UsageDelta(nil), n.pendingDeltas...)
	n.mu.Unlock()

	if reportID == "" {
		deltas = n.usage.SnapshotAndReset()
		var err error
		reportID, err = xboardtsunami.NewReportID()
		if err != nil {
			n.usage.Restore(deltas)
			return fmt.Errorf("tsunami node: create report id: %w", err)
		}
		if len(deltas) > 0 {
			n.mu.Lock()
			n.pendingReportID = reportID
			n.pendingDeltas = append([]control.UsageDelta(nil), deltas...)
			n.mu.Unlock()
		}
	}

	if err := n.adapter.SubmitReport(ctx, reportID, deltas); err != nil {
		if len(deltas) == 0 {
			return err
		}
		return fmt.Errorf("tsunami node: submit traffic report %s: %w", reportID, err)
	}
	if len(deltas) > 0 {
		n.mu.Lock()
		if n.pendingReportID == reportID {
			n.pendingReportID = ""
			n.pendingDeltas = nil
		}
		n.mu.Unlock()
		log.Printf("tsunami node: reported traffic users=%d report_id=%s", len(deltas), reportID)
	}
	return nil
}

// Close stops polling, terminates active sessions so their leases are
// released, sends one final report, and is safe to call repeatedly.
func (n *Node) Close() error {
	var result error
	n.closeOnce.Do(func() {
		n.mu.Lock()
		n.closed = true
		cancel := n.cancel
		n.mu.Unlock()
		if cancel != nil {
			cancel()
		}

		n.runWG.Wait()

		n.dataPlaneMu.Lock()
		n.mu.Lock()
		runtime := n.runtime
		n.runtime = nil
		n.mu.Unlock()
		closeErr := stopRuntime(runtime)
		n.dataPlaneMu.Unlock()

		timeout := n.adapter.Config().RequestTimeout
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		reportErr := n.ReportUsage(ctx)
		cancel()
		result = errors.Join(closeErr, reportErr)
		close(n.done)
	})
	return result
}

// UserStore exposes the runtime user state for status and tests.
func (n *Node) UserStore() *control.UserStore {
	return n.users
}

// UsageTracker exposes the local unreported usage accumulator for status and tests.
func (n *Node) UsageTracker() *control.UsageTracker {
	return n.usage
}

// ConfigRevision returns the active XBoard data-plane revision.
func (n *Node) ConfigRevision() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.configRevision
}

func (n *Node) run(ctx context.Context) {
	usersTicker := time.NewTicker(n.adapter.Config().SyncInterval)
	configTicker := time.NewTicker(n.adapter.Config().ConfigInterval)
	reportTicker := time.NewTicker(n.adapter.Config().SyncInterval)
	defer usersTicker.Stop()
	defer configTicker.Stop()
	defer reportTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-usersTicker.C:
			if err := n.SyncUsers(ctx); err != nil {
				log.Printf("tsunami node: XBoard user sync failed; retaining previous users: %v", err)
			}
		case <-configTicker.C:
			if err := n.SyncConfig(ctx); err != nil {
				log.Printf("tsunami node: XBoard config sync failed; retaining current data plane: %v", err)
			}
		case <-reportTicker.C:
			if err := n.ReportUsage(ctx); err != nil {
				log.Printf("tsunami node: XBoard traffic report failed; retaining pending traffic: %v", err)
			}
		}
	}
}

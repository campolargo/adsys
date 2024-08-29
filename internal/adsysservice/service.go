package adsysservice

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/leonelquinteros/gotext"
	"github.com/ubuntu/adsys"
	"github.com/ubuntu/adsys/internal/adsysservice/actions"
	"github.com/ubuntu/adsys/internal/authorizer"
	"github.com/ubuntu/adsys/internal/consts"
	log "github.com/ubuntu/adsys/internal/grpc/logstreamer"
	"github.com/ubuntu/adsys/internal/policies"
	"github.com/ubuntu/adsys/internal/stdforward"
	"github.com/ubuntu/decorate"
	"google.golang.org/grpc"
)

// Cat forwards any messages from all requests to the client.
// Anything logged by the server on stdout, stderr or via the standard logger.
// Only one call at a time can be performed here.
func (s *Service) Cat(_ *adsys.Empty, stream adsys.Service_CatServer) (err error) {
	defer decorate.OnError(&err, gotext.Get("error while trying to display daemon output"))

	if err := s.authorizer.IsAllowedFromContext(stream.Context(), actions.ActionServiceManage); err != nil {
		return err
	}

	// Redirect stdout and stderr
	f := streamWriter{stream}
	remove, err := stdforward.AddStdoutWriter(f)
	if err != nil {
		return err
	}
	defer remove()
	remove, err = stdforward.AddStderrWriter(f)
	if err != nil {
		return err
	}
	defer remove()

	// Redirect all logs
	defer log.AddStreamToForward(stream)()

	<-stream.Context().Done()
	return nil
}

type streamWriter struct {
	grpc.ServerStream
}

func (ss streamWriter) Write(b []byte) (n int, err error) {
	return len(b), ss.SendMsg(&adsys.StringResponse{Msg: string(b)})
}

// Status returns internal daemon status to the client.
func (s *Service) Status(_ *adsys.Empty, stream adsys.Service_StatusServer) (err error) {
	defer decorate.OnError(&err, gotext.Get("error while getting daemon status"))

	if err := s.authorizer.IsAllowedFromContext(stream.Context(), authorizer.ActionAlwaysAllowed); err != nil {
		return err
	}

	state := s.state

	// Empty values: takes defaults from conf to avoid exposing too much data
	if state.dconfDir == "" {
		state.dconfDir = consts.DefaultDconfDir
	}
	if state.sudoersDir == "" {
		state.sudoersDir = consts.DefaultSudoersDir
	}
	if state.policyKitDir == "" {
		state.policyKitDir = consts.DefaultPolicyKitDir
	}
	if state.apparmorDir == "" {
		state.apparmorDir = consts.DefaultApparmorDir
	}

	timeout := gotext.Get("unknown")
	socket := gotext.Get("unknown")
	if s.daemon != nil {
		timeout = s.daemon.Timeout().String()
		sock := s.daemon.GetSocketAddr()
		if sock != "" {
			socket = sock
		}
	}

	adInfo := s.adc.GetInfo(stream.Context())

	timeLayout := "Mon Jan 2 15:04"

	nextRefresh := gotext.Get("unknown")
	if next, err := s.nextRefreshTime(); err == nil {
		nextRefresh = next.Format(timeLayout)
	} else {
		log.Warning(stream.Context(), err)
	}

	// FIXME: gotext.Get needs to have the arguments parsed.
	updateFmt := "%s" + gotext.Get(", updated on ") + "%s"
	updateMachine := gotext.Get("Machine, no gpo applied found")
	t, err := s.policyManager.LastUpdateFor(stream.Context(), "", true)
	if err == nil {
		updateMachine = fmt.Sprintf(updateFmt, gotext.Get("Machine"), t.Format(timeLayout))
	}

	updateUsers := fmt.Sprint(gotext.Get("Can't get connected users"))
	users, err := s.adc.ListUsers(stream.Context(), true)
	if err == nil {
		updateUsers = fmt.Sprint(gotext.Get("Connected users:"))
		for _, u := range users {
			if t, err := s.policyManager.LastUpdateFor(stream.Context(), u, false); err == nil {
				updateUsers = updateUsers + "\n  " + fmt.Sprintf(updateFmt, u, t.Format(timeLayout))
			} else {
				updateUsers = updateUsers + "\n  " + gotext.Get("%s, no gpo applied found", u)
			}
		}
		if len(users) == 0 {
			updateUsers = updateUsers + "\n  " + gotext.Get("None")
		}
	}

	ubuntuProStatus := gotext.Get("Ubuntu Pro subscription is not active on this machine. Rules belonging to the following policy types will not be applied:\n")
	proOnlyRules := slices.Clone(policies.ProOnlyRules)
	slices.Sort(proOnlyRules)
	ubuntuProStatus = ubuntuProStatus + "  - " + strings.Join(proOnlyRules, "\n  - ")

	subscriptionEnabled := s.policyManager.GetSubscriptionState(stream.Context())
	if subscriptionEnabled {
		ubuntuProStatus = gotext.Get("Ubuntu Pro subscription active.")
	}

	status := gotext.Get(`%s
%s
Next Refresh: %s

%s

Active Directory:
  %s

Daemon:
  Timeout after %s
  Listening on: %s
  Cache path: %s
  Run path: %s
  Dconf path: %s
  Sudoers path: %s
  PolicyKit path: %s
  Apparmor path: %s`, updateMachine, updateUsers, nextRefresh,
		ubuntuProStatus,
		strings.Join(strings.Split(adInfo, "\n"), "\n  "),
		timeout, socket, state.cacheDir, state.runDir, state.dconfDir,
		state.sudoersDir, state.policyKitDir, state.apparmorDir)

	if err := stream.Send(&adsys.StringResponse{
		Msg: status,
	}); err != nil {
		log.Warningf(stream.Context(), "couldn't send status to client: %v", err)
	}

	return nil
}

// Stop requests to stop the service once all connections are done. Force will shut it down immediately and drop
// existing connections.
func (s *Service) Stop(r *adsys.StopRequest, stream adsys.Service_StopServer) (err error) {
	defer decorate.OnError(&err, gotext.Get("error while trying to stop daemon"))

	if err := s.authorizer.IsAllowedFromContext(stream.Context(), actions.ActionServiceManage); err != nil {
		return err
	}

	go s.daemon.Quit(r.GetForce())
	return nil
}

// ListUsers returns the list of currently active users.
func (s *Service) ListUsers(r *adsys.ListUsersRequest, stream adsys.Service_ListUsersServer) (err error) {
	defer decorate.OnError(&err, gotext.Get("error while trying to get the list of active users"))

	if err := s.authorizer.IsAllowedFromContext(stream.Context(), authorizer.ActionAlwaysAllowed); err != nil {
		return err
	}

	users, err := s.adc.ListUsers(stream.Context(), r.GetActive())
	if err != nil {
		return err
	}

	if err := stream.Send(&adsys.StringResponse{
		Msg: strings.Join(users, " "),
	}); err != nil {
		log.Warningf(stream.Context(), "couldn't send service version to client: %v", err)
	}
	return nil
}

// nextRefreshTime returns next adsys schedule refresh call.
func (s Service) nextRefreshTime() (next *time.Time, err error) {
	defer decorate.OnError(&err, gotext.Get("error while trying to determine next refresh time"))

	if s.initSystemTime == nil {
		return nil, errors.New(gotext.Get("no boot system time found"))
	}

	const unit = "adsys-gpo-refresh.timer"

	timerUnit := s.bus.Object(consts.SystemdDbusRegisteredName,
		dbus.ObjectPath(fmt.Sprintf("%s/unit/%s",
			consts.SystemdDbusObjectPath,
			strings.ReplaceAll(strings.ReplaceAll(unit, ".", "_2e"), "-", "_2d"))))
	val, err := timerUnit.GetProperty(fmt.Sprintf("%s.NextElapseUSecMonotonic", consts.SystemdDbusTimerInterface))
	if err != nil {
		return nil, errors.New(gotext.Get("could not find %s unit on systemd bus: no GPO refresh scheduled? %v", unit, err))
	}
	nextRaw, ok := val.Value().(uint64)
	if !ok {
		return nil, errors.New(gotext.Get("invalid next GPO refresh value for %v: %v", val.Value(), err))
	}
	nextRaw = nextRaw * 1000 // time.Microsecond / time.Nanosecond
	if nextRaw > math.MaxInt64 {
		return nil, errors.New(gotext.Get("next refresh time should be int64: %d", nextRaw))
	}

	//nolint: gosec // We handle the overflow check above.
	nextRefresh := s.initSystemTime.Add(time.Duration(int64(nextRaw)))
	return &nextRefresh, nil
}

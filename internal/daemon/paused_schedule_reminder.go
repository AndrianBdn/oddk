package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/andrianbdn/oddk/internal/services"
)

// pausedScheduleReminderInterval is how often a suspended schedule is re-reported.
//
// Daily, not per-tick: the state persists until someone acts on it, and a
// reminder that arrives every minute is one an operator mutes — which would
// defeat the only push signal that a host is unprotected.
const pausedScheduleReminderInterval = 24 * time.Hour

// startPausedScheduleReminder notifies, once at startup and then daily, while
// any schedule is paused.
//
// A paused schedule means the deployment is taking NO snapshots. `snapshot
// apply` pauses deliberately — the restored oddk.db carries the source host's
// offsite settings, so running the schedule here would upload into, and expire
// objects from, a bucket the source may still own — but the danger is that the
// pause then becomes permanent by forgetfulness. `oddk checklist` reports it,
// and nobody runs `oddk checklist` unprompted; that gap is exactly what the
// 0.1.67 cron-failure notifications were added to close, and it applies here
// with more force, because a paused schedule produces no cron run to fail.
//
// Firing at startup is deliberate too: the first thing after a DR restore is
// `systemctl start oddk`, so the operator gets immediate confirmation that the
// pause is real and reaches their channels.
func (s *Server) startPausedScheduleReminder(ctx context.Context) {
	s.remindIfSchedulesPaused(ctx)

	ticker := time.NewTicker(pausedScheduleReminderInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Paused-schedule reminder shutting down")
			return
		case <-ticker.C:
			s.remindIfSchedulesPaused(ctx)
		}
	}
}

func (s *Server) remindIfSchedulesPaused(ctx context.Context) {
	subject, body, paused := s.pausedScheduleNotice()
	if !paused {
		return
	}
	// Always log, even with no channels configured: the journal is the one place
	// this is guaranteed to be visible, and a fresh DR host often has no
	// notification channels reachable yet.
	log.Print(strings.ReplaceAll(body, "\n", " | "))

	sender := services.NewNotificationSender(s.store.Notifications, s.store)
	if err := sender.SendToAll(ctx, subject, body); err != nil {
		// "no notifications configured" is the common case on a fresh install
		// and is not a failure worth shouting about.
		if !errors.Is(err, services.ErrNoNotificationsConfigured) {
			log.Printf("Warning: could not send paused-schedule reminder: %v", err)
		}
	}
}

// pausedScheduleNotice builds the reminder. Split out so it can be tested
// without a notification channel, and returns paused=false when there is
// nothing to say.
func (s *Server) pausedScheduleNotice() (subject, body string, paused bool) {
	plan, err := s.store.Snapshot.GetPlan()
	if err != nil {
		log.Printf("Warning: could not read the snapshot schedule for the paused-schedule check: %v", err)
		return "", "", false
	}
	pausedBackups, err := s.store.Cron.ListPausedPlans()
	if err != nil {
		log.Printf("Warning: could not read backup schedules for the paused-schedule check: %v", err)
		return "", "", false
	}

	snapshotPaused := plan != nil && plan.IsPaused()
	if !snapshotPaused && len(pausedBackups) == 0 {
		return "", "", false
	}

	displayName := s.store.KV.GetDisplayName()
	var b strings.Builder
	if snapshotPaused {
		subject = fmt.Sprintf("⏸️  ODDK: snapshot schedule is PAUSED (%s)", displayName)
		fmt.Fprintf(&b, "The deployment-wide snapshot schedule on %s is PAUSED, so NO snapshots are being taken and this host cannot currently be rebuilt from one.\n\n", displayName)
		fmt.Fprintf(&b, "Paused since: %s\n", plan.PausedAt.UTC().Format("2006-01-02 15:04 MST"))
		if plan.PausedReason != "" {
			fmt.Fprintf(&b, "Reason: %s\n", plan.PausedReason)
		}
		fmt.Fprintf(&b, "\nResume with:\n  oddk snapshot setup-cron --resume\n")
	} else {
		subject = fmt.Sprintf("⏸️  ODDK: backup schedules are PAUSED (%s)", displayName)
		fmt.Fprintf(&b, "Per-instance backup schedules on %s are PAUSED.\n", displayName)
	}

	if len(pausedBackups) > 0 {
		names := make([]string, 0, len(pausedBackups))
		for _, p := range pausedBackups {
			names = append(names, p.InstanceName)
		}
		fmt.Fprintf(&b, "\nPaused backup schedules: %s\n", strings.Join(names, ", "))
		fmt.Fprintf(&b, "Resume each with:\n  oddk backup setup-cron --instance <name> --resume\n")
	}

	b.WriteString("\nIf this is a disaster-recovery rehearsal or a staged migration and the SOURCE host is still live, leaving these paused is correct — the restored offsite settings point at the source's bucket, and resuming here would upload into it and run retention against it. Point this host at its own bucket with 'oddk offsite apply' before resuming.\n")
	b.WriteString("\nThis reminder repeats daily until the schedules are resumed or removed.")

	return subject, b.String(), true
}

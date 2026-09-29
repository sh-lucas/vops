// Package sdnotify is the part of systemd's sd_notify protocol vops uses: READY=1 and watchdog pings.
// Everything is a no-op outside systemd (NOTIFY_SOCKET unset).
package sdnotify

import (
	"context"
	"log"
	"net"
	"os"
	"strconv"
	"time"
)

// Notify sends a state line ("READY=1", "WATCHDOG=1") to systemd.
func Notify(state string) error {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil
	}
	if addr[0] == '@' {
		addr = "\x00" + addr[1:] // abstract socket
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write([]byte(state))
	return err
}

// Watchdog pings systemd every half WatchdogSec while healthy() succeeds, so a hung process gets restarted,
// not only a dead one. Returns at once when the unit has no watchdog.
func Watchdog(ctx context.Context, healthy func(context.Context) error) {
	usec, _ := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if usec <= 0 || os.Getenv("NOTIFY_SOCKET") == "" {
		return
	}
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return
	}
	every := time.Duration(usec) * time.Microsecond / 2
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		check, cancel := context.WithTimeout(ctx, every)
		err := healthy(check)
		cancel()
		if err == nil {
			Notify("WATCHDOG=1")
		} else if ctx.Err() == nil {
			log.Printf("watchdog: not healthy: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

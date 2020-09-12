//  Written in 2014 by Matthieu Rakotojaona <matthieu.rakotojaona {on}
//  gmail.com>
//
//  To the extent possible under law, the author(s) have dedicated all
//  copyright and related and neighboring rights to this software to the
//  public domain worldwide. This software is distributed without any
//  warranty.
//
//  You should have received a copy of the CC0 Public Domain Dedication
//  along with this software. If not, see
//  <http://creativecommons.org/publicdomain/zero/1.0/>.

package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Kunde21/paclan/config"
	"github.com/Kunde21/paclan/peers"
	"github.com/Kunde21/paclan/server"
	"github.com/sirupsen/logrus"
)

func main() {
	const PaclanConfig = `/etc/pacman.d/paclan.conf`
	var confFile string
	flag.StringVar(&confFile, "c", PaclanConfig, "paclan configuration file")
	flag.Parse()
	log := logrus.New().WithField("service", "paclan")
	conf, err := config.New(confFile, flag.Args())
	if err != nil {
		log.Fatal(err)
	}
	log.WithFields(logrus.Fields{
		"arch":      conf.Arch,
		"cache_dir": conf.CacheDir,
		"sync_dir":  conf.SyncDir,
	}).Info("configured")
	peers := peers.New(conf)
	srv, err := server.New(conf, peers, log)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		log.WithField("port", conf.HTTPPort).Info("serving")
		if err := srv.ListenAndServe(); err != nil {
			log.WithError(err).Error("server failed")
		}
	}()
	peers.Serve(ctx)

	c := make(chan os.Signal)
	signal.Notify(c, syscall.SIGTERM, os.Kill)
	select {
	case <-ctx.Done():
	case <-c:
	}
	peers.Close()
	shCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	log.Info("http server closing")
	srv.Shutdown(shCtx)
	log.Info("exiting...")
}

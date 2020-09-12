package peers

import (
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type peerMap struct {
	*sync.Mutex
	peers  map[string]time.Time
	expire time.Duration
}

func newPeerMap(timeout time.Duration) peerMap {
	p := peerMap{
		Mutex:  &sync.Mutex{},
		peers:  make(map[string]time.Time),
		expire: timeout,
	}
	return p
}

func (p peerMap) add(log *logrus.Entry, peer string) {
	p.Lock()
	if _, ok := p.peers[peer]; !ok {
		log.WithFields(logrus.Fields{
			"peer":  peer,
			"count": len(p.peers) + 1,
		}).Info("registered")
	}
	// always update to reflect last seen timestamp
	p.peers[peer] = time.Now().UTC()
	p.Unlock()
}

func (p peerMap) GetPeerList() []string {
	p.Lock()
	peers := make([]string, 0, len(p.peers))
	for peer, t := range p.peers {
		if time.Since(t) > p.expire {
			// remove expired
			delete(p.peers, peer)
			continue
		}
		peers = append(peers, peer)
	}
	p.Unlock()
	return peers
}

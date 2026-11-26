//go:build !linux

package uot

func (r *Relay) udpToStream() {
	r.udpToStreamSingle()
}

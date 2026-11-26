//go:build !linux

package uot

func (r *Relay) streamToUDP() {
	r.streamToUDPSingle()
}

func (r *Relay) udpToStream() {
	r.udpToStreamSingle()
}

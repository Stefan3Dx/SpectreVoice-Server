package main

// Quality levels, the same numbers in the server and the apps.
//
// Voice (Opus over WebRTC, 48 kHz, 20 ms frames, in-band FEC always on):
//
//	1 Low       16 kbit/s mono, voice, DTX     (bad networks)
//	2 Standard  32 kbit/s mono, voice, DTX
//	3 High      64 kbit/s mono, voice          (the default)
//	4 Music     96 kbit/s stereo, music
//	5 Studio   160 kbit/s stereo, music
//	6 Max      256 kbit/s stereo, music
//
// Video (screen and camera; H.264 by default, VP9 / AV1 when both ends have them):
//
//	1 360p30 0.8 Mbit/s · 2 480p30 1.2 · 3 720p30 2.5 · 4 720p60 4 · 5 1080p30 5
//	6 1080p60 8 · 7 1440p60 12 · 8 2160p60 20 
//
// A channel has its voice level (0: the server's default); the apps encode at min(channel, their own
// setting). The server forwards packets as they are (no transcoding), so quality costs it bandwidth, not CPU.

type audioLevel struct {
	Level  int    `json:"level"`
	Kbps   int    `json:"kbps"`
	Stereo bool   `json:"stereo,omitempty"`
	Music  bool   `json:"music,omitempty"`
	DTX    bool   `json:"dtx,omitempty"`
	Name   string `json:"name"`
}

type videoLevel struct {
	Level  int    `json:"level"`
	Height int    `json:"height"`
	FPS    int    `json:"fps"`
	Kbps   int    `json:"kbps"`
	Name   string `json:"name"`
}

var audioLevels = []audioLevel{
	{1, 16, false, false, true, "Low"},
	{2, 32, false, false, true, "Standard"},
	{3, 64, false, false, false, "High"},
	{4, 96, true, true, false, "Music"},
	{5, 160, true, true, false, "Studio"},
	{6, 256, true, true, false, "Max"},
}

var videoLevels = []videoLevel{
	{1, 360, 30, 800, "360p30"},
	{2, 480, 30, 1200, "480p30"},
	{3, 720, 30, 2500, "720p30"},
	{4, 720, 60, 4000, "720p60"},
	{5, 1080, 30, 5000, "1080p30"},
	{6, 1080, 60, 8000, "1080p60"},
	{7, 1440, 60, 12000, "1440p60"},
	{8, 2160, 60, 20000, "2160p60"},
}

const defaultAudioLevel = 3

// clampAudio keeps a channel's level within 0 (default) .. the top.
func clampAudio(l, top int) int {
	if l < 0 {
		return 0
	}
	return min(l, top)
}

// channelAudio: the level a channel's voice is encoded at (its own, or the server's default).
func (s *server) channelAudio(c *channel) int {
	l := c.Quality
	if l == 0 {
		l = s.cfg.audioLevel
	}
	return clampAudio(max(1, l), s.limits().AudioMax)
}

// maxUsers: how many may be connected at once: the server's own setting, under its places (30, plus what donations add).
func (s *server) maxUsers() int {
	n, t := s.cfg.maxUsers, s.limits().MaxUsers
	switch {
	case n <= 0:
		return t // 0 = no limit
	case t > 0 && t < n:
		return t
	}
	return n
}

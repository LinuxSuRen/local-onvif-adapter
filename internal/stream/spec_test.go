package stream

import (
	"testing"

	"github.com/linuxsuren/local-onvif-adapter/internal/config"
)

func TestSpecIncludesAudio(t *testing.T) {
	base := config.Camera{Type: config.TypeDShow, Source: "WL24A"}
	withAudio := base
	withAudio.AudioEnabled = true
	withAudio.AudioSource = "Mic"
	if Spec(base) == Spec(withAudio) {
		t.Fatal("audio enabled must change spec (runner restart)")
	}
	otherMic := withAudio
	otherMic.AudioSource = "Mic2"
	if Spec(withAudio) == Spec(otherMic) {
		t.Fatal("audio source must change spec (runner restart)")
	}
}

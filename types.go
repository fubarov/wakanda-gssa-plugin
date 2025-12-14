package wakanda_gssa_plugin

import "github.com/fubarov/gssa-sdk"

type StreamItem struct {
	Title         string
	InfoHash      string
	Size          float64
	Seeders       int
	BgAudio       bool
	Resolution    string
	FileIdx       *int
	BehaviorHints *gssa_sdk.BehaviorHints
}

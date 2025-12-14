package regexps

import "regexp"

var Resolution = regexp.MustCompile(`(?i)\b(480p|720p|1080p|2160p|4K|8K)\b`)
var BgAudio = regexp.MustCompile(`(?i)bg[-\s.]?audio`)

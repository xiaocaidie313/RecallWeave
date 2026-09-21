package store

type source string
type sourceID int
type sourceMap map[source]sourceID

const (
	wx        source = "wx"
	qq        source = "qq"
	weibo     source = "weibo"
	twitter   source = "twitter"
	facebook  source = "facebook"
	instagram source = "instagram"
	youtube   source = "youtube"
	tiktok    source = "tiktok"
)

const (
	wxID        sourceID = 1
	qqID        sourceID = 2
	weiboID     sourceID = 3
	twitterID   sourceID = 4
	facebookID  sourceID = 5
	instagramID sourceID = 6
	youtubeID   sourceID = 7
	tiktokID    sourceID = 8
)

var SourceMap = sourceMap{
	wx:        wxID,
	qq:        qqID,
	weibo:     weiboID,
	twitter:   twitterID,
	facebook:  facebookID,
	instagram: instagramID,
	youtube:   youtubeID,
	tiktok:    tiktokID,
}

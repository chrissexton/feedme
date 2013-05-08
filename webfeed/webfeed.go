package webfeed

import (
	"bytes"
	"encoding/xml"
	"io"
	"time"

	"code.google.com/p/go.net/html"
)

type Feed struct {
	Title   string
	Link    string
	Updated time.Time
	Entries []Entry
}

type Entry struct {
	Title string
	Link  string
	// Summary is a valid HTML or escaped HTML summary of the entry.
	Summary []byte
	// Contents is the main contents of the entry in valid HTML or escaped HTML.
	Content []byte
	When    time.Time
}

// Read reads a feed from an io.Reader and returns it or an error if one was encountered.
//
// RSS is like the wild west with respect to time. When reading RSS, this
// function may return the non-fatal error ErrBadTime containing the
// first unparsable time encountered.
func Read(r io.Reader) (Feed, error) {
	var f feed
	if err := xml.NewDecoder(r).Decode(&f); err != nil {
		return Feed{}, err
	}
	if f.Rss.Title != "" {
		return rssFeed(f.Rss)
	}
	return atomFeed(f)
}

// ErrBadTime is a string containing a time that was not parsable.
type ErrBadTime string

func (e ErrBadTime) Error() string {
	return "Unable to parse time: " + string(e)
}

func rssFeed(r rss) (Feed, error) {
	updated, err := rssTime(r.Updated)
	f := Feed{
		Title:   r.Title,
		Link:    r.Link,
		Updated: updated,
	}

	for _, it := range r.Items {
		when, e := rssTime(it.Updated)
		if err == nil && e != nil {
			err = e
		}
		ent := Entry{
			Title:   it.Title,
			Link:    it.Link,
			Summary: fixHtml(it.Description),
			Content: fixHtml(it.Content.Data),
			When:    when,
		}
		f.Entries = append(f.Entries, ent)
	}
	return f, err
}

// RssTimeFormats is a slice of various time formats encountered in the wild.
var rssTimeFormats = []string{
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"Mon, 2 Jan 06 15:04:05 -0700",
	"02 January 2006",
}

// RssTime tries parsing a string using a variety of different time formats.
// If the string could not be parsed then the zero time is returned with an ErrBadTime error.
func rssTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}

	for _, f := range rssTimeFormats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}

	return time.Time{}, ErrBadTime(s)
}

func atomFeed(a feed) (Feed, error) {
	f := Feed{
		Title:   a.Title,
		Link:    a.link(),
		Updated: a.Updated,
	}

	for _, ent := range a.Entries {
		e := Entry{
			Title:   ent.Title,
			Link:    ent.Link.Href,
			Summary: fixHtml(ent.Summary),
			When:    ent.Updated,
		}
		if len(ent.Content) > 0 {
			e.Content = fixHtml(ent.Content[0].Data())
		}
		f.Entries = append(f.Entries, e)
	}
	return f, nil
}

// Feed is an intermediate representation used to unmarshall the XML;
// it can represent both an Atom feed an an RSS feed.  After unmarshalling
// this information is moved into a more "clean" format: the exported Feed.
type feed struct {
	Title   string      `xml:"title"`
	Links   []atomLink  `xml:"link"`
	Updated time.Time   `xml:"updated"`
	Author  []string    `xml:"author>name"`
	Id      string      `xml:"id"`
	Entries []atomEntry `xml:"entry"`
	Rss     rss         `xml:"channel"`
}

func (f *feed) link() string {
	for _, l := range f.Links {
		if l.Rel == "" || l.Rel == "alternate" {
			return l.Href
		}
	}
	return ""
}

type atomEntry struct {
	Title   string        `xml:"title"`
	Link    atomLink      `xml:"link"`
	Id      string        `xml:"id"`
	Updated time.Time     `xml:"updated"`
	Author  []string      `xml:"author>name"`
	Summary []byte        `xml:"summary"`
	Content []atomContent `xml:"content"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
}

type atomContent struct {
	Type     string `xml:"type,attr"`
	Contents []byte `xml:",innerxml"`
}

func (c atomContent) Data() []byte {
	unesc := c.Contents
	if c.Type != "xhtml" {
		unesc = []byte(html.UnescapeString(string(c.Contents)))
	}
	return unesc
}

type rss struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description []byte    `xml:"description"`
	Items       []rssItem `xml:"item"`

	// RSS uses its own time format (not understood by the XML parser, because it
	// is apparently a different format from all of the rest of XML in all the land).  We
	// read it as a string and parse it later.

	Updated string `xml:"pubDate"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description []byte `xml:"description"`

	// Content contains <content:encoded>, an extension used by Ars Technica's feeds.
	Content rssContent `xml:"content encoded"`
	Updated string     `xml:"pubDate"`
}

type rssContent struct {
	Data []byte `xml:",chardata"`
}

// FixHtml parses bytes as HTML and returns well-formed HTML if the parse
// was successful, or escaped HTML, if not.
func fixHtml(wild []byte) (well []byte) {
	n, err := html.Parse(bytes.NewReader(wild))
	if err != nil {
		return []byte(html.EscapeString(string(wild)))
	}

	defer func() {
		if err := recover(); err == bytes.ErrTooLarge {
			well = []byte(html.EscapeString(string(wild)))
		} else if err != nil {
			panic(err)
		}
	}()
	buf := bytes.NewBuffer(make([]byte, 0, len(wild)*2))
	if err := html.Render(buf, n); err != nil {
		return []byte(html.EscapeString(string(wild)))
	}

	well = buf.Bytes()
	openBody := []byte("<body>")
	i := bytes.Index(well, openBody)
	if i < 0 {
		return []byte(html.EscapeString(string(wild)))
	}
	well = well[i+len(openBody):]

	closeBody := []byte("</body>")
	i = bytes.Index(well, closeBody)
	if i < 0 {
		return []byte(html.EscapeString(string(wild)))
	}
	return well[:i]
}

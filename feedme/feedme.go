package feedme

import (
	"appengine"
	"appengine/datastore"
	"appengine/taskqueue"
	"appengine/user"
	"encoding/xml"
	"fmt"
	"html/template"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
)

var (
	templateFiles = []string{
		"tmplt/navbar.html",
		"tmplt/feed.html",
		"tmplt/manage.html",
		"tmplt/article.html",
		"tmplt/articles.html",
	}

	funcs = template.FuncMap{
		"dateTime": func(t time.Time) string { return t.Format("2006-01-02 15:04:05") },
		"stringEq": func(a, b string) bool { return a == b },
	}

	templates = template.Must(template.New("t").Funcs(funcs).ParseFiles(templateFiles...))
)

const (
	latestDuration = 18 * time.Hour
)

func init() {
	http.HandleFunc("/list", handleList)
	http.HandleFunc("/addopml", handleOpml)
	http.HandleFunc("/update", handleUpdate)
	http.HandleFunc("/refresh", handleRefresh)
	http.HandleFunc("/refreshAll", handleRefreshAll)
	http.HandleFunc("/", handleRoot)
}

type feedListEntry struct {
	Title      string
	Url        string
	LastFetch  time.Time
	EncodedKey string
}

func (f feedListEntry) Fresh() bool {
	return time.Since(f.LastFetch) < maxCacheDuration
}

// feedListEntrys is a type for sorting the infos.
type feedList []feedListEntry

func (u feedList) Len() int {
	return len(u)
}

func (u feedList) Less(i, j int) bool {
	return strings.ToLower(u[i].Title) < strings.ToLower(u[j].Title)
}

func (u feedList) Swap(i, j int) {
	u[i], u[j] = u[j], u[i]
}

func handleList(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/list" || r.Method != "GET" {
		http.NotFound(w, r)
		return
	}

	c := appengine.NewContext(r)

	var page struct {
		Title  string
		User   UserInfo
		Logout string
		Feeds  feedList
	}
	page.Title = "Feeds"

	var err error
	page.User, err = getUserInfo(c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	infos := make([]FeedInfo, len(page.User.Feeds))
	if err = datastore.GetMulti(c, page.User.Feeds, infos); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for i := range infos {
		page.Feeds = append(page.Feeds, feedListEntry{
			Title:      infos[i].Title,
			Url:        infos[i].Url,
			LastFetch:  infos[i].LastFetch,
			EncodedKey: page.User.Feeds[i].Encode(),
		})
	}

	sort.Sort(page.Feeds)

	page.Logout, err = user.LogoutURL(c, "/")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := templates.ExecuteTemplate(w, "manage.html", page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)

	uinfo, err := getUserInfo(c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if len(uinfo.Feeds) == 0 {
		http.Redirect(w, r, "/list", http.StatusFound)
		return
	}

	var feedPage = struct {
		Logout   string
		Title    string
		Link     string
		Errors   []error
		Articles Articles
	}{}

	feedPage.Logout, err = user.LogoutURL(c, "/")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if r.URL.Path == "/" {
		feedPage.Title = "Latest Articles"
		feedPage.Articles, feedPage.Errors = articlesSince(c, uinfo, time.Now().Add(-latestDuration))
	} else if r.URL.Path == "/all" {
		feedPage.Title = "All Articles"
		feedPage.Articles, feedPage.Errors = articlesSince(c, uinfo, time.Time{})
	} else {
		var key *datastore.Key
		var err error
		if key, err = datastore.DecodeKey(path.Base(r.URL.Path)); err != nil {
			http.NotFound(w, r)
			return
		}

		var f FeedInfo
		if err = datastore.Get(c, key, &f); err != nil {
			err = fmt.Errorf("%s: failed to load from the datastore: %s", key.StringID(), err.Error())
			feedPage.Errors = []error{err}

		} else {
			feedPage.Title = f.Title
			feedPage.Link = f.Link
			feedPage.Articles, err = f.articlesSince(c, time.Time{})
			if err != nil {
				feedPage.Errors = []error{err}
			}
		}
	}

	c.Debugf("%d articles\n", len(feedPage.Articles))
	sort.Sort(feedPage.Articles)

	if err := templates.ExecuteTemplate(w, "articles.html", feedPage); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func articlesSince(c appengine.Context, uinfo UserInfo, t time.Time) (articles Articles, errs []error) {
	for _, key := range uinfo.Feeds {
		var f FeedInfo
		if err := datastore.Get(c, key, &f); err != nil {
			err = fmt.Errorf("%s: failed to load from the datastore: %s", key.StringID(), err.Error())
			errs = append(errs, err)
			continue
		}
		as, err := f.articlesSince(c, t)
		if err != nil {
			err = fmt.Errorf("%s: failed to read articles: %s", f.Url, err.Error())
			errs = append(errs, err)
			continue
		}
		articles = append(articles, as...)
	}
	return
}

type Outline struct {
	XmlURL   string     `xml:"xmlUrl,attr"`
	Outlines []*Outline `xml:"outline"`
}

func handleOpml(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.NotFound(w, r)
		return
	}

	c := appengine.NewContext(r)

	f, _, err := r.FormFile("opml")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var b struct {
		Body Outline `xml:"body"`
	}
	err = xml.NewDecoder(f).Decode(&b)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	urls := opmlWalk(&b.Body, nil)

	c.Debugf("Got %d URLs from OPML", len(urls))

	for _, url := range urls {
		c.Debugf("opml %s", url)
		f, err := checkUrl(c, url)
		if err != nil {
			http.Error(w, "failed to check URL "+url+": "+err.Error(), http.StatusInternalServerError)
			return
		}

		if err = subscribe(c, f); err != nil {
			http.Error(w, "failed to subscribe "+url+": "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	http.Redirect(w, r, "/list", http.StatusFound)
}

func opmlWalk(r *Outline, urls []string) []string {
	if r.XmlURL != "" {
		urls = append(urls, r.XmlURL)
	}
	for _, kid := range r.Outlines {
		urls = append(urls, opmlWalk(kid, nil)...)
	}
	return urls
}

type errorList []error

func (es errorList) Error() string {
	s := ""
	for _, e := range es {
		s += e.Error() + "\n"
	}
	return s
}

func handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.NotFound(w, r)
		return
	}

	c := appengine.NewContext(r)
	u, err := getUserInfo(c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	curFeeds := make(map[string]bool)
	for _, f := range u.Feeds {
		curFeeds[f.StringID()] = true
	}

	var errs errorList

	urls := strings.Split(r.FormValue("urls"), "\n")
	for _, url := range urls {
		url = strings.TrimSpace(url)
		if len(url) == 0 {
			continue
		}
		if curFeeds[url] {
			delete(curFeeds, url)
		} else {
			c.Debugf("Subscribing to [%s]", url)
			f, err := checkUrl(c, url)
			if err != nil {
				err = fmt.Errorf("Failed to read %s: %s", url, err.Error())
				errs = append(errs, err)
				continue
			}
			if err := subscribe(c, f); err != nil {
				err = fmt.Errorf("Failed to subscribe to %s: %s", url, err.Error())
				errs = append(errs, err)
			}
		}
	}

	for url := range curFeeds {
		k := datastore.NewKey(c, feedKind, url, 0, nil)
		c.Debugf("Unsubscribing from [%s]", url)
		if err := unsubscribe(c, k); err != nil {
			err = fmt.Errorf("Failed to unsubscribe from %s: %s", url, err.Error())
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		http.Error(w, errs.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/list", http.StatusFound)
}

func handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.NotFound(w, r)
		return
	}

	k, err := datastore.DecodeKey(r.FormValue("feed"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	c := appengine.NewContext(r)
	c.Debugf("refreshing %s\n", k)

	var f FeedInfo
	if err = datastore.Get(c, k, &f); err != nil {
		http.Error(w, k.StringID()+" failed to load from the datastore: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err = f.ensureFresh(c); err != nil {
		http.Error(w, f.Url+" failed to refresh: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusResetContent)
}

func handleRefreshAll(w http.ResponseWriter, r *http.Request) {
	var errs errorList
	c := appengine.NewContext(r)
	for it := datastore.NewQuery(feedKind).KeysOnly().Run(c); ; {
		k, err := it.Next(nil)
		if err == datastore.Done {
			break
		} else if err != nil {
			errs = append(errs, err)
			continue
		}

		c.Debugf("adding a task to refresh %s\n", k)
		t := taskqueue.NewPOSTTask("/refresh", map[string][]string{"feed": {k.Encode()}})
		if _, err := taskqueue.Add(c, t, ""); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		http.Error(w, errs.Error(), http.StatusInternalServerError)
	}
	return
}

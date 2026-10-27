package server

import (
	"net/http"
	"strconv"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/service"
)

var weekdayNames = [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

type personalVM struct {
	M           service.PersonalMetrics
	HourBars    []service.NamedCount
	WeekdayBars []service.NamedCount
}

// metricsBodyView backs a metrics page body (#metrics-body).
type metricsBodyView struct {
	User         *auth.User
	Tab          string // reading | library | users
	Label        string
	Days         int
	P            personalVM              // reading / users pages
	O            service.OverviewMetrics // library page
	Users        []auth.User             // users page picker
	SelectedUser int64                   // users page
	SelectedName string                  // users page
}

type metricsLink struct{ Tab, Label string }

var metricsTabs = []metricsLink{{"reading", "Your reading"}, {"library", "Library & system"}, {"users", "Users"}}

// metricsLinks lists the metrics pages a user may open, for the sidebar.
func metricsLinks(u *auth.User) []metricsLink {
	var out []metricsLink
	for _, l := range metricsTabs {
		if metricsTabAllowed(u, l.Tab) {
			out = append(out, l)
		}
	}
	return out
}

// metricsDays clamps the window selector to the supported values.
func metricsDays(r *http.Request) int {
	switch r.URL.Query().Get("days") {
	case "90":
		return 90
	case "365":
		return 365
	default:
		return 30
	}
}

func metricsTabAllowed(u *auth.User, tab string) bool {
	switch tab {
	case "reading":
		return u.Can(auth.PermReaderUse)
	case "library":
		return u.Can(auth.PermStatsView)
	case "users":
		return u.Can(auth.PermMetricsUsers)
	}
	return false
}

func (u *webUI) buildPersonalFor(r *http.Request, userID int64) (personalVM, error) {
	m, err := u.svc.PersonalMetrics(r.Context(), userID, metricsDays(r), userFrom(r.Context()).AllowAdult)
	if err != nil {
		return personalVM{}, err
	}
	vm := personalVM{M: m}
	for h := 0; h < 24; h++ {
		vm.HourBars = append(vm.HourBars, service.NamedCount{Name: strconv.Itoa(h), Count: m.HourHist[h]})
	}
	for w := 0; w < 7; w++ {
		vm.WeekdayBars = append(vm.WeekdayBars, service.NamedCount{Name: weekdayNames[w], Count: m.WeekdayHist[w]})
	}
	return vm, nil
}

// buildMetricsBody assembles one metrics page; callers check the user may see it.
func (u *webUI) buildMetricsBody(r *http.Request, tab string) (metricsBodyView, error) {
	user := userFrom(r.Context())
	v := metricsBodyView{User: user, Tab: tab, Days: metricsDays(r)}
	for _, l := range metricsTabs {
		if l.Tab == tab {
			v.Label = l.Label
		}
	}
	switch tab {
	case "reading":
		p, err := u.buildPersonalFor(r, user.ID)
		if err != nil {
			return v, err
		}
		v.P = p
	case "library":
		o, err := u.svc.OverviewMetrics(r.Context(), user.AllowAdult)
		if err != nil {
			return v, err
		}
		v.O = o
	case "users":
		all, _ := u.svc.Auth().ListUsers(r.Context())
		for _, us := range all {
			if us.ID != user.ID {
				v.Users = append(v.Users, us)
			}
		}
		v.SelectedUser, _ = strconv.ParseInt(r.URL.Query().Get("user"), 10, 64)
		if v.SelectedUser > 0 {
			for _, us := range v.Users {
				if us.ID == v.SelectedUser {
					v.SelectedName = us.Username
				}
			}
			if v.SelectedName == "" {
				v.SelectedUser = 0 // unknown user id
			} else {
				p, err := u.buildPersonalFor(r, v.SelectedUser)
				if err != nil {
					return v, err
				}
				v.P = p
			}
		}
	}
	return v, nil
}

func (u *webUI) metricsHome(w http.ResponseWriter, r *http.Request) {
	links := metricsLinks(userFrom(r.Context()))
	if len(links) == 0 {
		writeError(w, http.StatusForbidden, "no metrics available for your role")
		return
	}
	http.Redirect(w, r, "/metrics/"+links[0].Tab, http.StatusFound)
}

func (u *webUI) metricsPage(w http.ResponseWriter, r *http.Request) {
	tab := r.PathValue("tab")
	if !metricsTabAllowed(userFrom(r.Context()), tab) {
		writeError(w, http.StatusForbidden, "missing permission for this metrics page")
		return
	}
	v, err := u.buildMetricsBody(r, tab)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	u.page(w, r, "metrics", v.Label, v)
}

func (u *webUI) trackerCalls(w http.ResponseWriter, r *http.Request) {
	if !userFrom(r.Context()).Can(auth.PermStatsView) {
		writeError(w, http.StatusForbidden, "missing permission for this metrics tab")
		return
	}
	u.frag(w, "trackerCalls", u.svc.TrackerCallDetails(r.Context()))
}

func (u *webUI) memoryStats(w http.ResponseWriter, r *http.Request) {
	if !userFrom(r.Context()).Can(auth.PermStatsView) {
		writeError(w, http.StatusForbidden, "missing permission for this metrics tab")
		return
	}
	u.frag(w, "memoryStats", u.svc.MemoryStats(r.Context()))
}

func (u *webUI) metricsBody(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	if !metricsTabAllowed(userFrom(r.Context()), tab) {
		writeError(w, http.StatusForbidden, "missing permission for this metrics page")
		return
	}
	v, err := u.buildMetricsBody(r, tab)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	u.frag(w, "metrics", v)
}

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

// metricsBodyView backs the tabbed metrics body (#metrics-body).
type metricsBodyView struct {
	User         *auth.User
	Tab          string // you | overview | users
	Days         int
	CanPersonal  bool
	CanOverview  bool
	CanUsers     bool
	P            personalVM              // you / users tabs
	O            service.OverviewMetrics // overview tab
	Users        []auth.User             // users tab picker
	SelectedUser int64                   // users tab
	SelectedName string                  // users tab
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
	case "you":
		return u.Can(auth.PermReaderUse)
	case "overview":
		return u.Can(auth.PermStatsView)
	case "users":
		return u.Can(auth.PermMetricsUsers)
	}
	return false
}

func defaultMetricsTab(u *auth.User) string {
	for _, tab := range []string{"you", "overview", "users"} {
		if metricsTabAllowed(u, tab) {
			return tab
		}
	}
	return ""
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

// buildMetricsBody assembles the active tab, falling back to the first the user
// may see. Returns ok=false when the user can see no tab at all.
func (u *webUI) buildMetricsBody(r *http.Request, tab string) (metricsBodyView, bool, error) {
	user := userFrom(r.Context())
	if tab == "" || !metricsTabAllowed(user, tab) {
		tab = defaultMetricsTab(user)
	}
	v := metricsBodyView{
		User:        user,
		Tab:         tab,
		Days:        metricsDays(r),
		CanPersonal: user.Can(auth.PermReaderUse),
		CanOverview: user.Can(auth.PermStatsView),
		CanUsers:    user.Can(auth.PermMetricsUsers),
	}
	switch tab {
	case "you":
		p, err := u.buildPersonalFor(r, user.ID)
		if err != nil {
			return v, true, err
		}
		v.P = p
	case "overview":
		o, err := u.svc.OverviewMetrics(r.Context(), user.AllowAdult)
		if err != nil {
			return v, true, err
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
					return v, true, err
				}
				v.P = p
			}
		}
	default:
		return v, false, nil
	}
	return v, true, nil
}

func (u *webUI) metricsPage(w http.ResponseWriter, r *http.Request) {
	v, ok, err := u.buildMetricsBody(r, r.URL.Query().Get("tab"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "no metrics available for your role")
		return
	}
	u.page(w, r, "metrics", "Metrics", v)
}

func (u *webUI) trackerCalls(w http.ResponseWriter, r *http.Request) {
	if !userFrom(r.Context()).Can(auth.PermStatsView) {
		writeError(w, http.StatusForbidden, "missing permission for this metrics tab")
		return
	}
	u.frag(w, "trackerCalls", u.svc.TrackerCallDetails(r.Context()))
}

func (u *webUI) metricsBody(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	user := userFrom(r.Context())
	if tab != "" && !metricsTabAllowed(user, tab) {
		writeError(w, http.StatusForbidden, "missing permission for this metrics tab")
		return
	}
	v, ok, err := u.buildMetricsBody(r, tab)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "no metrics available for your role")
		return
	}
	u.frag(w, "metrics", v)
}

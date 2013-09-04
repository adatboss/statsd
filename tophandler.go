package main

var topHandler = Logger{NewSession(PrefixRouter(map[string]Handler{
	"*":            Static("./web-client/app"),
	"/users":       usersRouter,
	"/groups":      groupsRouter,
	"/login":       &CheckMethod{"POST", &Transactional{HandlerFunc(login)}},
	"/logout":      &CheckMethod{"POST", HandlerFunc(logout)},
	"/whoami":      &CheckMethod{"GET", &Transactional{HandlerFunc(whoami)}},
	"/permissions": permissionsRouter,
	"/dashboards":  dashboardsRouter,
	"/widgets":     WidgetRouter,
	"/services": PrefixRouter(map[string]Handler{
		"/json": JsonServiceRouter,
	}),
}))}

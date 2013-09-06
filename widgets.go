package main

import (
	"admin/access"
	"admin/uuid"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Model

type Widget struct {
	Tx        *sql.Tx
	Id        string
	Type      string
	Dashboard string
	Created   time.Time
	Config    interface{}
}

var ErrNoDashboard = errors.New("No such dashboard")

func Widgets(tx *sql.Tx, dashboard string) ([]*Widget, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if dashboard != "" {
		if !uuid.Valid(dashboard) {
			return make([]*Widget, 0), nil
		}
		rows, err = tx.Query(`
			SELECT * FROM widgets
			WHERE dashboard = $1`,
			dashboard)
	} else {
		rows, err = tx.Query(`SELECT * FROM widgets`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]*Widget, 0)
	for rows.Next() {
		var (
			id, typ, dashboard string
			created            time.Time
			configSl           []byte
			config             interface{}
		)

		err := rows.Scan(&id, &typ, &dashboard, &created, &configSl)
		if err != nil {
			return nil, err
		}

		err = json.Unmarshal(configSl, &config)
		if err != nil {
			return nil, err
		}

		result = append(result, &Widget{
			Tx:        tx,
			Id:        id,
			Type:      typ,
			Dashboard: dashboard,
			Created:   created,
			Config:    config,
		})
	}

	return result, nil
}

func (w *Widget) Create() error {
	if !dashboardExists(w.Tx, w.Dashboard) {
		return ErrNoDashboard
	}

	config, err := json.Marshal(w.Config)
	if err != nil {
		return err
	}

	var id string
	if id, err = uuid.New4(); err != nil {
		return err
	}

	_, err = w.Tx.Exec(`
		INSERT INTO widgets(id, type, dashboard, created, config)
		VALUES ($1, $2, $3, NOW(), $4)`,
		id, w.Type, w.Dashboard, config)
	if err != nil {
		return err
	}

	w.Id = id
	return nil
}

func (w *Widget) Load() error {
	rows := w.Tx.QueryRow(`
		SELECT type, dashboard, created, config
		FROM widgets
		WHERE id = $1`, w.Id)

	var (
		typ, dashboard string
		configSl       []byte
		created        time.Time
		config         interface{}
	)
	err := rows.Scan(&typ, &dashboard, &created, &configSl)
	if err != nil {
		return err
	}

	err = json.Unmarshal(configSl, &config)
	if err != nil {
		return err
	}

	w.Type = typ
	w.Dashboard = dashboard
	w.Created = created
	w.Config = config
	return nil
}

func (w *Widget) Update() error {
	if !dashboardExists(w.Tx, w.Dashboard) {
		return ErrNoDashboard
	}

	config, err := json.Marshal(w.Config)
	if err != nil {
		return err
	}

	_, err = w.Tx.Exec(`
		UPDATE widgets
		SET type = $1, dashboard = $2, config = $3
		WHERE id = $4`,
		w.Type, w.Dashboard, config, w.Id)
	return err
}

func (w *Widget) Delete() error {
	_, err := w.Tx.Exec(`DELETE FROM widgets WHERE id = $1`, w.Id)
	return err
}

// Routing

var WidgetRouter = &Transactional{PrefixRouter{
	"/": &MethodRouter{
		"GET":  HandlerFunc(getWidgets),
		"POST": HandlerFunc(postWidget),
	},
	"*uuid": MethodRouter{
		"GET":    HandlerFunc(getWidget),
		"DELETE": HandlerFunc(deleteWidget),
		"PATCH":  HandlerFunc(patchWidget),
	},
}}

// Controllers

func getWidgets(t *Task) {
	if !access.HasPermission(t.Tx, t.Uid, "GET", "widgets", "") {
		t.Rw.WriteHeader(http.StatusForbidden)
		return
	}

	widgets, err := Widgets(t.Tx, t.Rq.URL.Query().Get("dashboard"))
	if err != nil {
		panic(err)
	}

	response := make([]interface{}, 0)
	for _, w := range widgets {
		response = append(response, map[string]interface{}{
			"id":        w.Id,
			"type":      w.Type,
			"dashboard": w.Dashboard,
			"created":   w.Created.Format("2006-01-02 15:04:05"),
			"config":    w.Config,
		})
	}
	t.SendJsonObject("widgets", response)
}

func postWidget(t *Task) {
	if !access.HasPermission(t.Tx, t.Uid, "POST", "widget", "") {
		t.Rw.WriteHeader(http.StatusForbidden)
		return
	}

	var (
		typ, dashboard string
		config         interface{}
		json           map[string]interface{}
		ok             bool
	)

	if json, ok = t.RecvJson().(map[string]interface{}); !ok {
		t.SendError("Invalid JSON parse")
		return
	}

	json = json["widget"].(map[string]interface{})

	if typ, ok = json["type"].(string); !ok {
		t.SendError("Invalid type")
		return
	}
	if dashboard, ok = json["dashboard"].(string); !ok {
		t.SendError("Invalid dashboard")
		return
	}

	w := &Widget{
		Tx:        t.Tx,
		Type:      typ,
		Dashboard: dashboard,
		Config:    config,
	}
	err := w.Create()
	if err == ErrNoDashboard {
		t.SendError("No such dashboard")
		return
	} else if err != nil {
		panic(err)
	}

	t.SendJsonObject("id", w.Id)
}

func getWidget(t *Task) {
	if !access.HasPermission(t.Tx, t.Uid, "GET", "widget", t.UUID) {
		t.Rw.WriteHeader(http.StatusForbidden)
		return
	}

	widget := &Widget{Tx: t.Tx, Id: t.UUID}
	if err := widget.Load(); err == sql.ErrNoRows {
		t.Rw.WriteHeader(http.StatusNotFound)
		return
	} else if err != nil {
		panic(err)
	}

	t.SendJsonObject("widget", map[string]interface{}{
		"id":       widget.Id,
		"type":     widget.Type,
		"dahboard": widget.Dashboard,
		"created":  widget.Created.Format("2006-01-02 15:04:05"),
		"config":   widget.Config,
	})
}

func deleteWidget(t *Task) {
	if !access.HasPermission(t.Tx, t.Uid, "DELETE", "widget", t.UUID) {
		t.Rw.WriteHeader(http.StatusForbidden)
		return
	}

	w := &Widget{Tx: t.Tx, Id: t.UUID}
	if err := w.Load(); err == sql.ErrNoRows {
		t.Rw.WriteHeader(http.StatusNotFound)
		return
	} else if err != nil {
		panic(err)
	}

	if err := w.Delete(); err != nil {
		panic(err)
	}
}

func patchWidget(t *Task) {
	if !access.HasPermission(t.Tx, t.Uid, "PATCH", "widget", "") {
		t.Rw.WriteHeader(http.StatusForbidden)
		return
	}

	w := &Widget{Tx: t.Tx, Id: t.UUID}
	if err := w.Load(); err == sql.ErrNoRows {
		t.Rw.WriteHeader(http.StatusNotFound)
		return
	} else if err != nil {
		panic(err)
	}

	json, ok := t.RecvJson().(map[string]interface{})
	if !ok {
		t.SendError("Invalid JSON")
		return
	}

	if _, ok := json["type"]; ok {
		if w.Type, ok = json["type"].(string); !ok {
			t.SendError("Invalid JSON")
			return
		}
	}
	if _, ok := json["dashboard"]; ok {
		if w.Dashboard, ok = json["dashboard"].(string); !ok {
			t.SendError("Invalid JSON")
			return
		}
		if !dashboardExists(t.Tx, w.Dashboard) {
			t.SendError("No such dashboard")
			return
		}
	}
	if config, ok := json["config"]; ok {
		w.Config = config
	}

	if err := w.Update(); err != nil {
		panic(err)
	}
}

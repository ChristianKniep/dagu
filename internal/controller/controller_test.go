package controller_test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yohamta/dagu/internal/controller"
	"github.com/yohamta/dagu/internal/dag"
	"github.com/yohamta/dagu/internal/database"
	"github.com/yohamta/dagu/internal/models"
	"github.com/yohamta/dagu/internal/scheduler"
	"github.com/yohamta/dagu/internal/settings"
	"github.com/yohamta/dagu/internal/sock"
	"github.com/yohamta/dagu/internal/utils"
)

var (
	testdataDir = path.Join(utils.MustGetwd(), "./testdata")
)

func TestMain(m *testing.M) {
	tempDir := utils.MustTempDir("controller_test")
	settings.ChangeHomeDir(tempDir)
	code := m.Run()
	os.RemoveAll(tempDir)
	os.Exit(code)
}

func testDAG(name string) string {
	return path.Join(testdataDir, name)
}

func TestGetStatus(t *testing.T) {
	file := testDAG("success.yaml")
	dr := controller.NewDAGReader()
	d, err := dr.ReadDAG(file, false)
	require.NoError(t, err)

	st, err := controller.New(d.DAG).GetStatus()
	require.NoError(t, err)
	assert.Equal(t, scheduler.SchedulerStatus_None, st.Status)
}

func TestGetStatusRunningAndDone(t *testing.T) {
	file := testDAG("status.yaml")

	dr := controller.NewDAGReader()
	dag, err := dr.ReadDAG(file, false)
	require.NoError(t, err)

	socketServer, _ := sock.NewServer(
		&sock.Config{
			Addr: dag.DAG.SockAddr(),
			HandlerFunc: func(w http.ResponseWriter, r *http.Request) {
				status := models.NewStatus(
					dag.DAG, []*scheduler.Node{},
					scheduler.SchedulerStatus_Running, 0, nil, nil)
				w.WriteHeader(http.StatusOK)
				b, _ := status.ToJson()
				w.Write(b)
			},
		})
	go func() {
		socketServer.Serve(nil)
	}()
	defer socketServer.Shutdown()

	time.Sleep(time.Millisecond * 100)
	st, _ := controller.New(dag.DAG).GetStatus()
	require.Equal(t, scheduler.SchedulerStatus_Running, st.Status)

	socketServer.Shutdown()

	st, _ = controller.New(dag.DAG).GetStatus()
	require.Equal(t, scheduler.SchedulerStatus_None, st.Status)
}

func TestGetDAG(t *testing.T) {
	file := testDAG("get_dag.yaml")
	dr := controller.NewDAGReader()
	dag, err := dr.ReadDAG(file, false)
	require.NoError(t, err)
	assert.Equal(t, "get_dag", dag.DAG.Name)
}

func TestGrepDAGs(t *testing.T) {
	ret, _, err := controller.GrepDAGs(testdataDir, "aabbcc")
	println(fmt.Sprintf("%v", ret))
	require.NoError(t, err)
	require.Equal(t, 1, len(ret))

	ret, _, err = controller.GrepDAGs(testdataDir, "steps")
	require.NoError(t, err)
	require.Greater(t, len(ret), 1)
}

func TestGetDAGList(t *testing.T) {
	dags, errs, err := controller.GetDAGs(testdataDir)
	require.NoError(t, err)
	require.Equal(t, 0, len(errs))

	matches, _ := filepath.Glob(path.Join(testdataDir, "*.yaml"))
	assert.Equal(t, len(matches), len(dags))
}

func TestUpdateStatus(t *testing.T) {
	file := testDAG("update_status.yaml")

	dr := controller.NewDAGReader()
	dag, err := dr.ReadDAG(file, false)
	require.NoError(t, err)
	req := "test-update-status"
	now := time.Now()

	db := &database.Database{
		Config: database.DefaultConfig(),
	}
	w, _, _ := db.NewWriter(dag.DAG.Location, now, req)
	err = w.Open()
	require.NoError(t, err)

	st := newStatus(dag.DAG, req,
		scheduler.SchedulerStatus_Success, scheduler.NodeStatus_Success)

	err = w.Write(st)
	require.NoError(t, err)
	w.Close()

	time.Sleep(time.Millisecond * 100)

	st, err = controller.New(dag.DAG).GetStatusByRequestId(req)
	require.NoError(t, err)
	require.Equal(t, scheduler.NodeStatus_Success, st.Nodes[0].Status)

	st.Nodes[0].Status = scheduler.NodeStatus_Error
	err = controller.New(dag.DAG).UpdateStatus(st)
	require.NoError(t, err)

	updated, err := controller.New(dag.DAG).GetStatusByRequestId(req)
	require.NoError(t, err)

	require.Equal(t, 1, len(st.Nodes))
	require.Equal(t, scheduler.NodeStatus_Error, updated.Nodes[0].Status)
}

func TestUpdateStatusFailure(t *testing.T) {
	file := testDAG("update_status_failed.yaml")

	dr := controller.NewDAGReader()
	dag, err := dr.ReadDAG(file, false)
	require.NoError(t, err)
	req := "test-update-status-failure"

	socketServer, _ := sock.NewServer(
		&sock.Config{
			Addr: dag.DAG.SockAddr(),
			HandlerFunc: func(w http.ResponseWriter, r *http.Request) {
				st := newStatus(dag.DAG, req,
					scheduler.SchedulerStatus_Running, scheduler.NodeStatus_Success)
				w.WriteHeader(http.StatusOK)
				b, _ := st.ToJson()
				w.Write(b)
			},
		})
	go func() {
		socketServer.Serve(nil)
	}()
	defer socketServer.Shutdown()

	st := newStatus(dag.DAG, req,
		scheduler.SchedulerStatus_Error, scheduler.NodeStatus_Error)
	err = controller.New(dag.DAG).UpdateStatus(st)
	require.Error(t, err)

	st.RequestId = "invalid request id"
	err = controller.New(dag.DAG).UpdateStatus(st)
	require.Error(t, err)
}

func TestStart(t *testing.T) {
	file := testDAG("start_err.yaml")
	dr := controller.NewDAGReader()
	dag, err := dr.ReadDAG(file, false)
	require.NoError(t, err)

	c := controller.New(dag.DAG)
	err = c.Start(path.Join(utils.MustGetwd(), "../../bin/dagu"), "", "")
	require.Error(t, err)

	st, err := c.GetLastStatus()
	require.NoError(t, err)
	require.Equal(t, scheduler.SchedulerStatus_Error, st.Status)
}

func TestStartStop(t *testing.T) {
	file := testDAG("start_stop.yaml")
	dr := controller.NewDAGReader()
	dag, err := dr.ReadDAG(file, false)
	require.NoError(t, err)

	c := controller.New(dag.DAG)
	c.StartAsync(path.Join(utils.MustGetwd(), "../../bin/dagu"), "", "")

	require.Eventually(t, func() bool {
		st, _ := c.GetStatus()
		return st.Status == scheduler.SchedulerStatus_Running
	}, time.Millisecond*1500, time.Millisecond*100)

	c.Stop()

	require.Eventually(t, func() bool {
		st, _ := c.GetLastStatus()
		return st.Status == scheduler.SchedulerStatus_Cancel
	}, time.Millisecond*1500, time.Millisecond*100)
}

func TestRestart(t *testing.T) {
	file := testDAG("restart.yaml")
	dr := controller.NewDAGReader()
	dag, err := dr.ReadDAG(file, false)
	require.NoError(t, err)

	c := controller.New(dag.DAG)
	err = c.Restart(path.Join(utils.MustGetwd(), "../../bin/dagu"), "")
	require.NoError(t, err)

	st, err := c.GetLastStatus()
	require.NoError(t, err)
	require.Equal(t, scheduler.SchedulerStatus_Success, st.Status)
}

func TestRetry(t *testing.T) {
	file := testDAG("retry.yaml")
	dr := controller.NewDAGReader()
	dag, err := dr.ReadDAG(file, false)
	require.NoError(t, err)

	c := controller.New(dag.DAG)
	err = c.Start(path.Join(utils.MustGetwd(), "../../bin/dagu"), "", "x y z")
	require.NoError(t, err)

	s, err := c.GetLastStatus()
	require.NoError(t, err)
	require.Equal(t, scheduler.SchedulerStatus_Success, s.Status)

	err = c.Retry(path.Join(utils.MustGetwd(), "../../bin/dagu"), "", s.RequestId)
	require.NoError(t, err)
	s2, err := c.GetLastStatus()
	require.NoError(t, err)

	require.Equal(t, scheduler.SchedulerStatus_Success, s2.Status)
	require.Equal(t, s.Params, s2.Params)

	s3, err := c.GetStatusByRequestId(s2.RequestId)
	require.NoError(t, err)
	require.Equal(t, s2, s3)

	s4 := c.GetStatusHist(1)
	require.Equal(t, s2, s4[0].Status)
}

func TestSave(t *testing.T) {
	tmpDir := utils.MustTempDir("controller-test-save")
	defer os.RemoveAll(tmpDir)
	d := &dag.DAG{
		Name:     "test",
		Location: path.Join(tmpDir, "test.yaml"),
	}

	c := controller.New(d)

	// invalid config
	dat := `name: test DAG`
	err := c.Save(dat)
	require.Error(t, err)

	// valid config
	dat = `name: test DAG
steps:
  - name: "1"
    command: "true"
`
	err = c.Save(dat)
	require.Error(t, err) // no config file

	// create file
	f, _ := utils.CreateFile(d.Location)
	defer f.Close()

	err = c.Save(dat)
	require.NoError(t, err)

	// check file
	saved, _ := os.Open(d.Location)
	defer saved.Close()
	b, _ := io.ReadAll(saved)
	require.Equal(t, dat, string(b))
}

func TestRemove(t *testing.T) {
	tmpDir := utils.MustTempDir("controller-test-remove")
	defer os.RemoveAll(tmpDir)
	d := &dag.DAG{
		Name:     "test",
		Location: path.Join(tmpDir, "test.yaml"),
	}

	c := controller.New(d)

	dat := `name: test DAG
steps:
  - name: "1"
    command: "true"
`
	// create file
	f, _ := utils.CreateFile(d.Location)
	defer f.Close()

	err := c.Save(dat)
	require.NoError(t, err)

	// check file
	saved, _ := os.Open(d.Location)
	defer saved.Close()
	b, _ := io.ReadAll(saved)
	require.Equal(t, dat, string(b))

	// remove file
	err = c.Delete()
	require.NoError(t, err)
	require.NoFileExists(t, d.Location)
}

func TestNewConfig(t *testing.T) {
	tmpDir := utils.MustTempDir("controller-test-save")
	defer os.RemoveAll(tmpDir)

	// invalid filename
	filename := path.Join(tmpDir, "test")
	err := controller.NewConfig(filename)
	require.Error(t, err)

	// correct filename
	filename = path.Join(tmpDir, "test.yaml")
	err = controller.NewConfig(filename)
	require.NoError(t, err)

	// check file
	cl := &dag.Loader{}

	d, err := cl.Load(filename, "")
	require.NoError(t, err)
	require.Equal(t, "test", d.Name)

	steps := d.Steps[0]
	require.Equal(t, "step1", steps.Name)
	require.Equal(t, "echo", steps.Command)
	require.Equal(t, []string{"hello"}, steps.Args)
}

func TestRenameConfig(t *testing.T) {
	tmpDir := utils.MustTempDir("controller-test-rename")
	defer os.RemoveAll(tmpDir)
	oldFile := path.Join(tmpDir, "test.yaml")
	newFile := path.Join(tmpDir, "test2.yaml")

	err := controller.NewConfig(oldFile)
	require.NoError(t, err)

	err = controller.RenameConfig(oldFile, "invalid-config-name")
	require.Error(t, err)

	err = controller.RenameConfig(oldFile, newFile)
	require.NoError(t, err)
	require.FileExists(t, newFile)
}

func newStatus(d *dag.DAG, reqId string,
	schedulerStatus scheduler.SchedulerStatus, nodeStatus scheduler.NodeStatus) *models.Status {
	n := time.Now()
	ret := models.NewStatus(
		d, []*scheduler.Node{
			{
				NodeState: scheduler.NodeState{
					Status: nodeStatus,
				},
			},
		},
		schedulerStatus, 0, &n, nil)
	ret.RequestId = reqId
	return ret
}

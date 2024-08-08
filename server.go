package main

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	docker "github.com/docker/docker/client"
	_ "github.com/mattn/go-sqlite3"
	"github.com/song940/duckci/templates"
	"gopkg.in/yaml.v2"
)

type DuckCI struct {
	db *Storage
}

type DuckCIConfig struct {
	Database string
}

type Storage struct {
	db *sql.DB
}

type Project struct {
	Id        uint32    `json:"id"`
	Name      string    `json:"name"`
	Repo      string    `json:"repo"`
	CreatedAt time.Time `json:"created_at"`
}

type Job struct {
	Id        uint32    `json:"id"`
	Branch    string    `json:"branch"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`

	Project `json:"project"`
}

type Log struct {
	Id        uint32    `json:"id"`
	Job       *Job      `json:"job"`
	Log       string    `json:"log"`
	CreatedAt time.Time `json:"created_at"`
}

type Step struct {
	Name  string `yaml:"name"`
	Image string `yaml:"image"`
	Runs  string `yaml:"runs"`
}

type Config struct {
	Steps []Step `yaml:"steps"`
}

type H map[string]interface{}

func New(config DuckCIConfig) (ci *DuckCI, err error) {
	storage, err := NewStorage(config.Database)
	if err != nil {
		return
	}
	ci = &DuckCI{
		db: storage,
	}
	storage.Init()
	return
}

func NewStorage(path string) (storage *Storage, err error) {
	db, err := sql.Open("sqlite3", path)
	storage = &Storage{db}
	return
}

func (s *Storage) Init() error {
	sql := `
		create table projects (
			id integer not null primary key,
			name text not null,
			repo text not null,
			created_at timestamp default CURRENT_TIMESTAMP
		);
		create table jobs (
			id integer not null primary key,
			project_id integer not null,
			branch text not null,
			status integer not null,
			created_at timestamp default CURRENT_TIMESTAMP,
			foreign key (project_id) references projects(id)
		);
		create table logs (
			id integer not null primary key,
			job_id integer not null,
			log text not null,
			created_at timestamp default CURRENT_TIMESTAMP,
			foreign key (job_id) references jobs(id)
		);
		`
	_, err := s.db.Exec(sql)
	return err
}

func (s *Storage) createProject(name string, repo string) (project Project, err error) {
	sql := `INSERT INTO projects (name, repo) VALUES (?, ?) RETURNING id`
	err = s.db.QueryRow(sql, name, repo).Scan(&project.Id)
	project.Name = name
	project.Repo = repo
	return
}

func (s *Storage) listProjects() (projects []Project, err error) {
	sql := `SELECT id, name, repo, created_at FROM projects`
	rows, err := s.db.Query(sql)
	if err != nil {
		return
	}
	defer rows.Close()

	var project Project
	for rows.Next() {
		err := rows.Scan(&project.Id, &project.Name, &project.Repo, &project.CreatedAt)
		if err != nil {
			log.Fatal(err)
		}
		projects = append(projects, project)
	}
	return
}

func (s *Storage) getProjectById(id string) (project Project, err error) {
	sql := `select id, name, repo, created_at from projects where id = ?`
	err = s.db.QueryRow(sql, id).Scan(&project.Id, &project.Name, &project.Repo, &project.CreatedAt)
	return
}

func (s *Storage) createJob(projectId, branch string) (job Job, err error) {
	sql := `INSERT INTO jobs (project_id, branch, status) VALUES (?, ?, ?) RETURNING id`
	err = s.db.QueryRow(sql, projectId, branch, -1).Scan(&job.Id)
	if err != nil {
		return
	}
	job, err = s.getJobById(fmt.Sprint(job.Id))
	return
}

func (s *Storage) getJobsByProjectId(id uint32) (jobs []Job, err error) {
	sql := `select id, branch, status, created_at from jobs where project_id = ? order by created_at desc`
	rows, err := s.db.Query(sql, id)
	if err != nil {
		return
	}
	var job Job
	for rows.Next() {
		err = rows.Scan(&job.Id, &job.Branch, &job.Status, &job.CreatedAt)
		if err != nil {
			return
		}
		jobs = append(jobs, job)
	}
	return
}

func (s *Storage) updateJobStatus(jobId uint32, status int64) {
	sql := `update jobs set status = ? where id = ?`
	_, err := s.db.Exec(sql, status, jobId)
	if err != nil {
		log.Fatal(err)
	}
}

func (s *Storage) inertLog(jobId uint32, t string) {
	sql := `insert into logs (job_id, log) values (?, ?)`
	_, err := s.db.Exec(sql, jobId, t)
	if err != nil {
		log.Fatal(err)
	}
}

func (s *Storage) getJobById(id string) (job Job, err error) {
	job.Project = Project{}
	sql := `
		SELECT 
			p.id, p.name, p.repo, 
			j.id, j.branch, j.status, j.created_at
		FROM jobs j, projects p 
		WHERE j.project_id = p.id and j.id = ?
	`
	err = s.db.QueryRow(sql, id).Scan(
		&job.Project.Id, &job.Project.Name, &job.Project.Repo,
		&job.Id, &job.Branch, &job.Status, &job.CreatedAt,
	)
	return
}

func (s *Storage) getLogsByJobId(jobId string) (logs []Log, err error) {
	sql := `select id, log, created_at from logs where job_id = ?`
	rows, err := s.db.Query(sql, jobId)
	var log Log
	for rows.Next() {
		err = rows.Scan(&log.Id, &log.Log, &log.CreatedAt)
		if err != nil {
			return
		}
		logs = append(logs, log)
	}
	return
}

// Render renders an HTML template with the provided data.
func (reader *DuckCI) Render(w http.ResponseWriter, templateName string, data H) {
	if data == nil {
		data = H{}
	}
	// tmpl, err := template.ParseFiles("templates/layout.html", "templates/"+templateName+".html")
	// Parse templates from embedded file system
	tmpl, err := template.New("").ParseFS(templates.Files, "layout.html", templateName+".html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Execute "index.html" within the layout and write to response
	err = tmpl.ExecuteTemplate(w, "layout", data)
	if err != nil {
		log.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (ci *DuckCI) IndexView(w http.ResponseWriter, r *http.Request) {
	projects, err := ci.db.listProjects()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ci.Render(w, "index", H{
		"projects": projects,
	})
}

func (ci *DuckCI) ProjectView(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && r.URL.Path == "/new" {
		ci.Render(w, "new", H{})
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/projects" {
		r.ParseForm()
		name := r.Form.Get("name")
		repo := r.Form.Get("repo")
		project, err := ci.db.createProject(name, repo)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/projects?id=%d", project.Id), http.StatusFound)
	}
	if r.Method == "GET" && r.URL.Path == "/projects" {
		projectId := r.URL.Query().Get("id")
		project, _ := ci.db.getProjectById(projectId)
		tasks, err := ci.db.getJobsByProjectId(project.Id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		ci.Render(w, "project", H{
			"project": project,
			"tasks":   tasks,
		})
		return
	}
}

func (ci *DuckCI) TaskView(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		taskId := r.URL.Query().Get("id")
		job, err := ci.db.getJobById(taskId)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logs, err := ci.db.getLogsByJobId(taskId)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		ci.Render(w, "task", H{
			"job":  job,
			"logs": logs,
		})
	}
	if r.Method == http.MethodPost {
		r.ParseForm()
		projectId := r.FormValue("project")
		branch := r.FormValue("branch")
		job, err := ci.db.createJob(projectId, branch)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		go ci.Run(&job)
		http.Redirect(w, r, fmt.Sprintf("/task?id=%d", job.Id), http.StatusFound)
	}
}

func (ci *DuckCI) PushLog(job *Job, s string) {
	ci.db.inertLog(job.Id, s)
	log.Println(s)
}

func (ci *DuckCI) Run(job *Job) {
	ci.PushLog(job, fmt.Sprintf("Starting job for project: %s", job.Project.Name))

	repoPath, err := ci.cloneRepository(job)
	if err != nil {
		ci.PushLog(job, fmt.Sprintf("Failed to clone repository: %v", err))
		ci.updateJobStatus(job, -1)
		return
	}

	config, err := ci.loadConfig(repoPath)
	if err != nil {
		ci.PushLog(job, fmt.Sprintf("Failed to load configuration: %v", err))
		ci.updateJobStatus(job, -1)
		return
	}

	client, err := docker.NewClientWithOpts(docker.FromEnv)
	if err != nil {
		ci.PushLog(job, fmt.Sprintf("Failed to create Docker client: %v", err))
		ci.updateJobStatus(job, -1)
		return
	}

	for i, step := range config.Steps {
		ci.PushLog(job, fmt.Sprintf("Starting step %d: %s", i+1, step.Name))

		err := ci.runStep(client, job, step, repoPath)
		if err != nil {
			ci.PushLog(job, fmt.Sprintf("Step %d failed: %v", i+1, err))
			ci.updateJobStatus(job, -1)
			return
		}

		ci.PushLog(job, fmt.Sprintf("Step %d completed successfully", i+1))
	}

	ci.PushLog(job, "Job completed successfully")
	ci.updateJobStatus(job, 0)
}

func (ci *DuckCI) cloneRepository(job *Job) (string, error) {
	repoPath := fmt.Sprintf("/tmp/duck-ci/%s-task-%d", job.Project.Name, job.Id)
	cmd := exec.Command("git", "clone", "-b", job.Branch, job.Project.Repo, repoPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git clone error: %v, output: %s", err, out)
	}
	ci.PushLog(job, string(out))
	return repoPath, nil
}

func (ci *DuckCI) loadConfig(repoPath string) (*Config, error) {
	configPath := filepath.Join(repoPath, "duck-ci.yml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}
	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", err)
	}
	return &config, nil
}

func (ci *DuckCI) runStep(client *docker.Client, job *Job, step Step, repoPath string) error {
	err := ci.ensureImage(client, step.Image)
	if err != nil {
		return fmt.Errorf("failed to ensure image: %v", err)
	}

	containerID, err := ci.createAndStartContainer(client, step, repoPath)
	if err != nil {
		return fmt.Errorf("failed to create and start container: %v", err)
	}

	err = ci.streamLogs(client, job, containerID)
	if err != nil {
		return fmt.Errorf("failed to stream logs: %v", err)
	}

	statusCode, err := ci.waitForContainer(client, containerID)
	if err != nil {
		return fmt.Errorf("failed to wait for container: %v", err)
	}

	err = client.ContainerRemove(context.Background(), containerID, types.ContainerRemoveOptions{})
	if err != nil {
		ci.PushLog(job, fmt.Sprintf("Warning: Failed to remove container: %v", err))
	}

	if statusCode != 0 {
		return fmt.Errorf("step failed with status code: %d", statusCode)
	}

	return nil
}

func (ci *DuckCI) ensureImage(client *docker.Client, imageName string) error {
	images, err := client.ImageList(context.Background(), types.ImageListOptions{})
	if err != nil {
		return err
	}

	for _, image := range images {
		for _, tag := range image.RepoTags {
			if tag == imageName {
				return nil
			}
		}
	}

	out, err := client.ImagePull(context.Background(), imageName, types.ImagePullOptions{})
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(io.Discard, out)
	return err
}

func (ci *DuckCI) createAndStartContainer(client *docker.Client, step Step, repoPath string) (string, error) {
	containerConfig := &container.Config{
		Image:      step.Image,
		WorkingDir: "/app",
		Cmd:        []string{"/bin/sh", "-c", step.Runs},
		Tty:        true,
	}

	hostConfig := &container.HostConfig{
		Binds: []string{fmt.Sprintf("%s:/app", repoPath)},
	}

	resp, err := client.ContainerCreate(context.Background(), containerConfig, hostConfig, nil, nil, "")
	if err != nil {
		return "", err
	}

	err = client.ContainerStart(context.Background(), resp.ID, types.ContainerStartOptions{})
	if err != nil {
		return "", err
	}

	return resp.ID, nil
}

func (ci *DuckCI) streamLogs(client *docker.Client, job *Job, containerID string) error {
	out, err := client.ContainerLogs(context.Background(), containerID, types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return err
	}
	defer out.Close()

	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		ci.PushLog(job, scanner.Text())
	}

	return scanner.Err()
}

func (ci *DuckCI) waitForContainer(client *docker.Client, containerID string) (int64, error) {
	statusCh, errCh := client.ContainerWait(context.Background(), containerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		return -1, err
	case status := <-statusCh:
		return status.StatusCode, nil
	}
}

func (ci *DuckCI) updateJobStatus(job *Job, status int64) {
	ci.db.updateJobStatus(job.Id, status)
}

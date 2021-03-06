package main

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/bacongobbler/kubed-generator-sdk-go/manifest"
	"github.com/bacongobbler/kubed-generator-sdk-go/pack"
)

const (
	environmentEnvVar = "KUBED_ENV"
	globalUsage       = `Generates boilerplate code that is necessary to write a nodejs app.
`
	dockerfile = `FROM node:8
RUN mkdir -p /usr/src/app
WORKDIR /usr/src/app

ARG NODE_ENV
ENV NODE_ENV $NODE_ENV
COPY package.json /usr/src/app/
RUN yarn install && yarn cache clean --force
COPY . /usr/src/app

ENV PORT 8080
EXPOSE 8080

CMD [ "yarn", "start" ]
`
	indexjs = `const http = require('http');
const port = process.env.PORT || 8080;

const requestHandler = (request, response) => {
  console.log(request.url);
  response.end("Hello World, I'm a Node.js app!\n");
}

const server = http.createServer(requestHandler);

server.listen(port, (err) => {
  if (err) {
    return console.log(err);
  }

  console.log(` + "`server is listening on ${port}`" + `);
})
`
	packagejson = `{
  "name": "example-nodejs",
  "version": "0.1.0",
  "main": "index.js",
  "scripts": {
    "start": "node index.js"
  }
}
`
	deploymentTemplate = `kind: Deployment
apiVersion: apps/v1
metadata:
  name: {{ template "{% .AppName %}.{% .Name %}.name" . }}
  labels:
    kubed: {{ template "{% .AppName %}.name" . }}
    component: {% .Name %}
    generator: nodejs
spec:
  selector:
    matchLabels:
      kubed: {{ template "{% .AppName %}.name" . }}
      component: {% .Name %}
  replicas: {{ default .Values.{% .Name %}.replicaCount 1 }}
  template:
    metadata:
      annotations:
        buildID: {{ .Values.buildID }}
      labels:
        kubed: {{ template "{% .AppName %}.name" . }}
        component: {% .Name %}
    spec:
      containers:
        - name: {% .Name %}
          image: "{{ .Values.{% .Name %}.image.repository }}:{{ .Values.{% .Name %}.image.tag }}"
          imagePullPolicy: {{ default .Values.{% .Name %}.image.pullPolicy "IfNotPresent" }}
          ports:
            - name: http
              containerPort: 8080
              protocol: TCP
`
	serviceTemplate = `kind: Service
apiVersion: v1
metadata:
  name: {{ template "{% .AppName %}.{% .Name %}.name" . }}
  labels:
    kubed: {{ template "{% .AppName %}.name" . }}
    component: {% .Name %}
    generator: nodejs
spec:
  selector:
    kubed: {{ template "{% .AppName %}.name" . }}
    component: {% .Name %}
  ports:
    - port: 80
      targetPort: http
      protocol: TCP
      name: http
`
	helperTemplate = `
{{- define "{% .AppName %}.{% .Name %}.name" -}}
{{- printf "%s-{% .Name %}" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
`
	valuesTemplate = `
{% .Name %}:
  image: {}
`
)

var flagDebug bool

type generateCmd struct {
	stdout         io.Writer
	name           string
	repositoryName string
}

func newRootCmd(stdout io.Writer, stdin io.Reader, stderr io.Writer) *cobra.Command {
	c := generateCmd{
		stdout: stdout,
	}

	cmd := &cobra.Command{
		Use:          "generator-nodejs <name>",
		Short:        "generates nodejs applications",
		Long:         globalUsage,
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if flagDebug {
				log.SetLevel(log.DebugLevel)
			}
			c.name = args[0]
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return c.run()
		},
	}

	pf := cmd.PersistentFlags()
	pf.BoolVar(&flagDebug, "debug", false, "enable verbose output")

	return cmd
}

func (c *generateCmd) run() error {
	var config manifest.Manifest
	tomlFilepath := filepath.Join("config", "kubed.toml")
	if _, err := toml.DecodeFile(tomlFilepath, &config); err != nil {
		return err
	}
	appConfig, found := config.Environments[defaultEnvironment()]
	if !found {
		return fmt.Errorf("Environment %v not found in %s", defaultEnvironment(), tomlFilepath)
	}

	deploymentFile, err := os.Create(filepath.Join("charts", appConfig.Name, "templates", fmt.Sprintf("%s-deployment.yaml", c.name)))
	if err != nil {
		return err
	}
	defer deploymentFile.Close()
	serviceFile, err := os.Create(filepath.Join("charts", appConfig.Name, "templates", fmt.Sprintf("%s-service.yaml", c.name)))
	if err != nil {
		return err
	}
	defer serviceFile.Close()
	valuesFile, err := os.OpenFile(filepath.Join("charts", appConfig.Name, "values.yaml"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer valuesFile.Close()
	helpersFile, err := os.OpenFile(filepath.Join("charts", appConfig.Name, "templates", "_helpers.tpl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer helpersFile.Close()

	// scaffold helm chart
	values := struct {
		AppName       string
		Name          string
		GeneratorName string
	}{
		AppName:       appConfig.Name,
		Name:          c.name,
		GeneratorName: "nodejs",
	}
	dt := template.Must(template.New("deployment").Delims("{%", "%}").Parse(deploymentTemplate))
	if err := dt.Execute(deploymentFile, values); err != nil {
		return err
	}

	st := template.Must(template.New("service").Delims("{%", "%}").Parse(serviceTemplate))
	if err := st.Execute(serviceFile, values); err != nil {
		return err
	}

	vt := template.Must(template.New("values").Delims("{%", "%}").Parse(valuesTemplate))
	if err := vt.Execute(valuesFile, values); err != nil {
		return err
	}

	ht := template.Must(template.New("helpers").Delims("{%", "%}").Parse(helperTemplate))
	if err := ht.Execute(helpersFile, values); err != nil {
		return err
	}

	// scaffold business logic
	if _, err := os.Stat(c.name); os.IsNotExist(err) {
		if err := os.Mkdir(c.name, 0777); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("there was an error checking if %s exists: %v", c.name, err)
	}

	p := &pack.Pack{
		Files: map[string]io.ReadCloser{
			"Dockerfile":   ioutil.NopCloser(bytes.NewBufferString(dockerfile)),
			"index.js":     ioutil.NopCloser(bytes.NewBufferString(indexjs)),
			"package.json": ioutil.NopCloser(bytes.NewBufferString(packagejson)),
		},
	}

	if err := p.SaveDir(c.name); err != nil {
		return err
	}

	// Each pack makes the assumption that they're listening on port 8080
	addRoute(filepath.Join("config", "routes"), fmt.Sprintf("/%s/\t%s\t8080", c.name, c.name))

	fmt.Fprintln(c.stdout, "--> Ready to sail")
	return nil
}

// addRoute adds a new route to fpath. It appends the route
// above the default route so that it takes higher priority
// in the list than the static files, but lower priority than
// other routes higher up in the list.
func addRoute(fpath, route string) error {
	const defaultRoute = "/\tstatic\t"
	b, err := ioutil.ReadFile(fpath)
	if err != nil {
		return err
	}
	content := string(b)
	fileContent := ""
	n, defaultRouteExists := containsDefaultRoute(content)
	if defaultRouteExists {
		for i, line := range strings.Split(content, "\n") {
			if i == n {
				fileContent += route + "\n"
			}
			fileContent += line + "\n"
		}
	} else {
		fileContent = content
		if !strings.HasSuffix(fileContent, "\n") && fileContent != "" {
			fileContent += "\n"
		}
		fileContent += route + "\n"
	}
	return ioutil.WriteFile(fpath, []byte(fileContent), 0644)
}

// containsDefaultRoute determines if the content contains a line starting with
//
// / static 8080 /
//
// if it does, it returns the line number (0-indexed) where the first instance
// of that route is found.
func containsDefaultRoute(content string) (int, bool) {
	for i, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 {
			if fields[0] == "/" && fields[1] == "static" &&
				fields[2] == "8080" && fields[3] == "/" {
				return i, true
			}
		}
	}
	return 0, false
}

func main() {
	cmd := newRootCmd(os.Stdout, os.Stdin, os.Stderr)
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func defaultEnvironment() string {
	env := os.Getenv(environmentEnvVar)
	if env == "" {
		env = manifest.DefaultEnvironmentName
	}
	return env
}

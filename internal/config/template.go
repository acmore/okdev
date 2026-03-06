package config

import "fmt"

const basicTemplate = `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: my-project
spec:
  namespace: default
  session:
    defaultNameTemplate: "{{ .Repo }}-{{ .Branch }}-{{ .User }}"
    ttlHours: 72
    idleTimeoutMinutes: 120
    shareable: true
    lockMode: none
  workspace:
    mountPath: /workspace
    pvc:
      size: 50Gi
  sync:
    engine: native
    syncthing:
      version: v1.29.7
      autoInstall: true
      image: ghcr.io/acmore/okdev-syncthing:v1.29.7
    paths:
      - .:/workspace
    exclude:
      - .git/
      - .venv/
      - node_modules/
  ports:
    - name: app
      local: 8080
      remote: 8080
  ssh:
    user: root
    remotePort: 22
    localPort: 2222
`

const gpuTemplate = `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: llm-project
spec:
  namespace: ai-dev
  session:
    defaultNameTemplate: "{{ .Repo }}-{{ .Branch }}-{{ .User }}"
    ttlHours: 72
    idleTimeoutMinutes: 120
    shareable: true
    lockMode: none
  workspace:
    mountPath: /workspace
    pvc:
      size: 200Gi
      storageClassName: fast-ssd
  sync:
    engine: native
    syncthing:
      version: v1.29.7
      autoInstall: true
      image: ghcr.io/acmore/okdev-syncthing:v1.29.7
    paths:
      - .:/workspace
    exclude:
      - .git/
      - .venv/
      - node_modules/
      - checkpoints/
      - data/
  ports:
    - name: api
      local: 8080
      remote: 8080
    - name: tensorboard
      local: 6006
      remote: 6006
  ssh:
    user: root
    remotePort: 22
    localPort: 2222
  podTemplate:
    spec:
      containers:
        - name: dev
          image: nvidia/cuda:12.4.1-devel-ubuntu22.04
          command: ["sleep", "infinity"]
          resources:
            requests:
              cpu: "8"
              memory: 32Gi
              nvidia.com/gpu: "1"
            limits:
              cpu: "16"
              memory: 64Gi
              nvidia.com/gpu: "1"
          volumeMounts:
            - name: workspace
              mountPath: /workspace
      volumes:
        - name: workspace
          persistentVolumeClaim:
            claimName: okdev-workspace
`

var DefaultTemplate = basicTemplate

func TemplateByName(name string) (string, error) {
	switch name {
	case "", "basic":
		return basicTemplate, nil
	case "gpu", "llm-gpu":
		return gpuTemplate, nil
	default:
		return "", fmt.Errorf("unknown template %q (supported: basic, gpu)", name)
	}
}

package config

import "fmt"

var basicTemplate = fmt.Sprintf(`apiVersion: okdev.io/v1alpha1
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
  workspace:
    mountPath: /workspace
    pvc:
      size: %s
  sync:
    engine: syncthing
    syncthing:
      version: %s
      autoInstall: true
      image: %s
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
`, DefaultWorkspacePVCSize, DefaultSyncthingVersion, DefaultSyncthingImage)

var gpuTemplate = fmt.Sprintf(`apiVersion: okdev.io/v1alpha1
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
  workspace:
    mountPath: /workspace
    pvc:
      size: 200Gi
      storageClassName: fast-ssd
  sync:
    engine: syncthing
    syncthing:
      version: %s
      autoInstall: true
      image: %s
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
`, DefaultSyncthingVersion, DefaultSyncthingImage)

var llmStackTemplate = fmt.Sprintf(`apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: llm-stack
spec:
  namespace: ai-dev
  session:
    defaultNameTemplate: "{{ .Repo }}-{{ .Branch }}-{{ .User }}"
    ttlHours: 72
    idleTimeoutMinutes: 120
    shareable: true
  workspace:
    mountPath: /workspace
    pvc:
      size: 200Gi
      storageClassName: fast-ssd
  sync:
    engine: syncthing
    syncthing:
      version: %s
      autoInstall: true
      image: %s
    paths:
      - .:/workspace
    exclude:
      - .git/
      - .venv/
      - node_modules/
      - checkpoints/
      - data/
  ports:
    - name: app
      local: 8080
      remote: 8080
    - name: redis
      local: 6379
      remote: 6379
    - name: qdrant
      local: 6333
      remote: 6333
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
          env:
            - name: REDIS_URL
              value: redis://127.0.0.1:6379
            - name: QDRANT_URL
              value: http://127.0.0.1:6333
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
        - name: redis
          image: redis:7-alpine
          args: ["--save", "", "--appendonly", "no"]
          ports:
            - containerPort: 6379
        - name: qdrant
          image: qdrant/qdrant:v1.13.2
          ports:
            - containerPort: 6333
      volumes:
        - name: workspace
          persistentVolumeClaim:
            claimName: okdev-workspace
`, DefaultSyncthingVersion, DefaultSyncthingImage)

var DefaultTemplate = basicTemplate

func TemplateByName(name string) (string, error) {
	switch name {
	case "", "basic":
		return basicTemplate, nil
	case "gpu", "llm-gpu":
		return gpuTemplate, nil
	case "llm-stack", "multi-container":
		return llmStackTemplate, nil
	default:
		return "", fmt.Errorf("unknown template %q (supported: basic, gpu, llm-stack)", name)
	}
}

package config

const DefaultTemplate = `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: my-project
spec:
  namespace: default
  session:
    defaultNameTemplate: "{{ .Repo }}-{{ .Branch }}-{{ .User }}"
  workspace:
    mountPath: /workspace
`

// main.go
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

type vError struct {
	line int
	msg  string
}

type errBag struct {
	file string
	list []vError
}

func (e *errBag) add(line int, msg string) { e.list = append(e.list, vError{line: line, msg: msg}) }

func (e *errBag) printAndExit() {
	if len(e.list) == 0 {
		return
	}
	// печатаем в STDOUT — так ожидают автотесты
	for _, er := range e.list {
		if er.line > 0 {
			fmt.Fprintf(os.Stdout, "%s:%d %s\n", e.file, er.line, er.msg)
		} else {
			fmt.Fprintf(os.Stdout, "%s: %s\n", e.file, er.msg)
		}
	}
	os.Exit(1)
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: yamlvalid <path-to-yaml>")
		os.Exit(2)
	}
	path := os.Args[1]
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stdout, "%s: cannot read file content: %v\n", filepath.Base(path), err)
		os.Exit(2)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		fmt.Fprintf(os.Stdout, "%s: cannot unmarshal file content: %v\n", filepath.Base(path), err)
		os.Exit(2)
	}

	bag := &errBag{file: filepath.Base(path)}
	for _, doc := range root.Content {
		validateTopLevel(doc, bag)
	}
	bag.printAndExit()
}

// ---------- helpers over yaml.Node ----------

func getMap(doc *yaml.Node) (map[string]*yaml.Node, *yaml.Node) {
	if doc.Kind != yaml.MappingNode {
		return nil, doc
	}
	m := make(map[string]*yaml.Node)
	for i := 0; i < len(doc.Content); i += 2 {
		k := doc.Content[i]
		v := doc.Content[i+1]
		m[k.Value] = v
	}
	return m, doc
}

func child(doc *yaml.Node, key string) (*yaml.Node, bool) {
	if doc.Kind != yaml.MappingNode {
		return nil, false
	}
	for i := 0; i < len(doc.Content); i += 2 {
		if doc.Content[i].Value == key {
			return doc.Content[i+1], true
		}
	}
	return nil, false
}

func isScalarString(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && (n.Tag == "!!str" || n.Tag == "") }
func isScalarInt(n *yaml.Node) bool    { return n.Kind == yaml.ScalarNode && n.Tag == "!!int" }

// ---------- validators ----------

func validateTopLevel(doc *yaml.Node, bag *errBag) {
	m, node := getMap(doc)
	if m == nil {
		bag.add(node.Line, "root must be object")
		return
	}

	// apiVersion
	api, ok := m["apiVersion"]
	if !ok {
		bag.add(0, "apiVersion is required")
	} else {
		if !isScalarString(api) {
			bag.add(api.Line, "apiVersion must be string")
		} else if api.Value != "v1" {
			bag.add(api.Line, fmt.Sprintf("apiVersion has unsupported value '%s'", api.Value))
		}
	}

	// kind
	kind, ok := m["kind"]
	if !ok {
		bag.add(0, "kind is required")
	} else {
		if !isScalarString(kind) {
			bag.add(kind.Line, "kind must be string")
		} else if kind.Value != "Pod" {
			bag.add(kind.Line, fmt.Sprintf("kind has unsupported value '%s'", kind.Value))
		}
	}

	// metadata
	meta, ok := m["metadata"]
	if !ok {
		bag.add(0, "metadata is required")
	} else {
		validateObjectMeta(meta, bag)
	}

	// spec
	spec, ok := m["spec"]
	if !ok {
		bag.add(0, "spec is required")
	} else {
		validatePodSpec(spec, bag)
	}
}

func validateObjectMeta(n *yaml.Node, bag *errBag) {
	m, node := getMap(n)
	if m == nil {
		bag.add(node.Line, "metadata must be object")
		return
	}

	// name (required, non-empty)
	name, ok := m["name"]
	if !ok {
		bag.add(0, "name is required")
	} else if !isScalarString(name) {
		bag.add(name.Line, "name must be string")
	} else if strings.TrimSpace(name.Value) == "" {
		// пустая строка — считаем как отсутствие обязательного поля
		bag.add(name.Line, "name is required")
	}

	// namespace (optional)
	if ns, ok := m["namespace"]; ok {
		if !isScalarString(ns) {
			bag.add(ns.Line, "namespace must be string")
		}
	}

	// labels (optional)
	if labels, ok := m["labels"]; ok {
		if labels.Kind != yaml.MappingNode {
			bag.add(labels.Line, "labels must be object")
		} else {
			for i := 0; i < len(labels.Content); i += 2 {
				k := labels.Content[i]
				v := labels.Content[i+1]
				if !isScalarString(k) || !isScalarString(v) {
					bag.add(v.Line, "labels must be object")
					break
				}
			}
		}
	}
}

func validatePodSpec(n *yaml.Node, bag *errBag) {
	m, node := getMap(n)
	if m == nil {
		bag.add(node.Line, "spec must be object")
		return
	}

	// os (optional)
	if osn, ok := m["os"]; ok {
		validatePodOS(osn, bag)
	}

	// containers (required)
	cont, ok := m["containers"]
	if !ok {
		bag.add(0, "containers is required")
	} else {
		if cont.Kind != yaml.SequenceNode {
			bag.add(cont.Line, "containers must be array")
		} else if len(cont.Content) == 0 {
			bag.add(cont.Line, "containers must be non-empty array")
		} else {
			seen := map[string]struct{}{}
			for _, c := range cont.Content {
				name := validateContainer(c, bag)
				if name != "" {
					if _, dup := seen[name]; dup {
						bag.add(c.Line, fmt.Sprintf("name has invalid format '%s'", name))
					}
					seen[name] = struct{}{}
				}
			}
		}
	}
}

// Поддерживаем:
// 1) os: "linux"|"windows"
// 2) os: { name: "linux"|"windows" }
func validatePodOS(n *yaml.Node, bag *errBag) {
	switch n.Kind {
	case yaml.ScalarNode:
		if !isScalarString(n) {
			bag.add(n.Line, "os must be string")
			return
		}
		val := strings.ToLower(n.Value)
		if val != "linux" && val != "windows" {
			bag.add(n.Line, fmt.Sprintf("os has unsupported value '%s'", n.Value))
		}
	case yaml.MappingNode:
		osName, ok := child(n, "name")
		if !ok {
			bag.add(0, "os.name is required")
			return
		}
		if !isScalarString(osName) {
			bag.add(osName.Line, "name must be string")
			return
		}
		val := strings.ToLower(osName.Value)
		if val != "linux" && val != "windows" {
			bag.add(osName.Line, fmt.Sprintf("os has unsupported value '%s'", osName.Value))
		}
	default:
		bag.add(n.Line, "os must be string")
	}
}

var reSnake = regexp.MustCompile(`^[a-z0-9]+(?:_[a-z0-9]+)*$`)
var reImage = regexp.MustCompile(`^registry\.bigbrother\.io\/[^:]+:[A-Za-z0-9._-]+$`)

func validateContainer(n *yaml.Node, bag *errBag) (nameOut string) {
	m, node := getMap(n)
	if m == nil {
		bag.add(node.Line, "container must be object")
		return ""
	}

	// name
	name, ok := m["name"]
	if !ok {
		bag.add(0, "name is required")
	} else {
		if !isScalarString(name) {
			bag.add(name.Line, "name must be string")
		} else if strings.TrimSpace(name.Value) == "" {
			// пустое имя — трактуем как отсутствие обязательного поля (ожидание автотеста)
			bag.add(name.Line, "name is required")
		} else if !reSnake.MatchString(name.Value) {
			bag.add(name.Line, fmt.Sprintf("name has invalid format '%s'", name.Value))
		}
		nameOut = name.Value
	}

	// image
	img, ok := m["image"]
	if !ok {
		bag.add(0, "image is required")
	} else if !isScalarString(img) {
		bag.add(img.Line, "image must be string")
	} else if !reImage.MatchString(img.Value) {
		bag.add(img.Line, fmt.Sprintf("image has invalid format '%s'", img.Value))
	}

	// ports
	if ports, ok := m["ports"]; ok {
		if ports.Kind != yaml.SequenceNode {
			bag.add(ports.Line, "ports must be array")
		} else {
			for _, p := range ports.Content {
				validateContainerPort(p, bag)
			}
		}
	}

	// probes
	if rp, ok := m["readinessProbe"]; ok {
		validateProbe(rp, bag, "readinessProbe")
	}
	if lp, ok := m["livenessProbe"]; ok {
		validateProbe(lp, bag, "livenessProbe")
	}

	// resources
	res, ok := m["resources"]
	if !ok {
		bag.add(0, "resources is required")
	} else {
		validateResourceRequirements(res, bag)
	}

	return nameOut
}

func validateContainerPort(n *yaml.Node, bag *errBag) {
	m, node := getMap(n)
	if m == nil {
		bag.add(node.Line, "ports item must be object")
		return
	}

	// containerPort
	cp, ok := m["containerPort"]
	if !ok {
		bag.add(0, "containerPort is required")
	} else {
		if !isScalarInt(cp) {
			bag.add(cp.Line, "containerPort must be int")
		} else {
			val, err := toInt(cp.Value)
			if err != nil || val < 1 || val > 65535 {
				bag.add(cp.Line, "containerPort value out of range")
			}
		}
	}

	// protocol
	if proto, ok := m["protocol"]; ok {
		if !isScalarString(proto) {
			bag.add(proto.Line, "protocol must be string")
		} else if proto.Value != "TCP" && proto.Value != "UDP" {
			bag.add(proto.Line, fmt.Sprintf("protocol has unsupported value '%s'", proto.Value))
		}
	}
}

func validateProbe(n *yaml.Node, bag *errBag, field string) {
	m, node := getMap(n)
	if m == nil {
		bag.add(node.Line, field+" must be object")
		return
	}
	get, ok := m["httpGet"]
	if !ok {
		bag.add(0, "httpGet is required")
		return
	}
	validateHTTPGet(get, bag)
}

func validateHTTPGet(n *yaml.Node, bag *errBag) {
	m, node := getMap(n)
	if m == nil {
		bag.add(node.Line, "httpGet must be object")
		return
	}

	// path
	p, ok := m["path"]
	if !ok {
		bag.add(0, "path is required")
	} else if !isScalarString(p) {
		bag.add(p.Line, "path must be string")
	} else if !strings.HasPrefix(p.Value, "/") {
		bag.add(p.Line, fmt.Sprintf("path has invalid format '%s'", p.Value))
	}

	// port
	pt, ok := m["port"]
	if !ok {
		bag.add(0, "port is required")
	} else if !isScalarInt(pt) {
		bag.add(pt.Line, "port must be int")
	} else {
		val, err := toInt(pt.Value)
		if err != nil || val < 1 || val > 65535 {
			bag.add(pt.Line, "port value out of range")
		}
	}
}

var reMem = regexp.MustCompile(`^\d+(Ki|Mi|Gi)$`)

func validateResourceRequirements(n *yaml.Node, bag *errBag) {
	m, node := getMap(n)
	if m == nil {
		bag.add(node.Line, "resources must be object")
		return
	}
	if lim, ok := m["limits"]; ok {
		validateResourceMap(lim, bag, "limits")
	}
	if req, ok := m["requests"]; ok {
		validateResourceMap(req, bag, "requests")
	}
}

func validateResourceMap(n *yaml.Node, bag *errBag, field string) {
	if n.Kind != yaml.MappingNode {
		bag.add(n.Line, field+" must be object")
		return
	}
	for i := 0; i < len(n.Content); i += 2 {
		k := n.Content[i]
		v := n.Content[i+1]
		if !isScalarString(k) {
			bag.add(v.Line, field+" must be object")
			continue
		}
		switch k.Value {
		case "cpu":
			if !isScalarInt(v) {
				bag.add(v.Line, "cpu must be int")
			}
		case "memory":
			if !isScalarString(v) {
				bag.add(v.Line, "memory must be string")
			} else if !reMem.MatchString(v.Value) {
				bag.add(v.Line, fmt.Sprintf("memory has invalid format '%s'", v.Value))
			}
		default:
			// лишние ключи игнорируем
		}
	}
}

// --------- small utils ----------

func toInt(s string) (int, error) {
	var x int
	_, err := fmt.Sscanf(s, "%d", &x)
	if err != nil {
		return 0, errors.New("not int")
	}
	return x, nil
}

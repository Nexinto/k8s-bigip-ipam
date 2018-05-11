package main

type F5Backend struct {
	ServiceName string `json:"serviceName,omitempty"`
	ServicePort int32  `json:"servicePort,omitempty"`
}

type F5VirtualAddress struct {
	BindAddr string `json:"bindAddr,omitempty"`
	Port     int32  `json:"port,omitempty"`
}

type F5SSLProfile struct {
	SSLProfileName  string   `json:"f5ProfileName,omitempty"`
	SSLProfileNames []string `json:"f5ProfileNames,omitempty"`
}

type F5Mode string

const (
	F5ModeHTTP F5Mode = "http"
	F5ModeTCP  F5Mode = "tcp"
)

type F5Frontend struct {
	Balance        string           `json:"balance,omitempty"`
	Mode           F5Mode           `json:"mode,omitempty"`
	Partition      string           `json:"partition,omitempty"`
	VirtualAddress F5VirtualAddress `json:"virtualAddress,omitempty"`
	SSLProfile     *F5SSLProfile    `json:"sslProfile,omitempty"`
}

type F5VirtualServer struct {
	Frontend F5Frontend `json:"frontend,omitempty"`
	Backend  F5Backend  `json:"backend,omitempty"`
}

type F5VirtualServerConfig struct {
	VirtualServer F5VirtualServer `json:"virtualServer,omitempty"`
}

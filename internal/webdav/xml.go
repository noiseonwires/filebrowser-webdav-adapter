package webdav

import "encoding/xml"

// XML structures for WebDAV protocol

// multistatus is the root element for multi-status responses.
type multistatus struct {
	XMLName   xml.Name           `xml:"D:multistatus"`
	XMLNS     string             `xml:"xmlns:D,attr"`
	Responses []propfindResponse `xml:"D:response"`
}

// propfindResponse represents a single response in a multistatus.
type propfindResponse struct {
	Href     string     `xml:"D:href"`
	Propstat []propstat `xml:"D:propstat"`
}

// propstat contains property status information.
type propstat struct {
	Prop   prop   `xml:"D:prop"`
	Status string `xml:"D:status"`
}

// prop contains the actual properties.
type prop struct {
	DisplayName      string        `xml:"D:displayname,omitempty"`
	GetContentLength string        `xml:"D:getcontentlength,omitempty"`
	GetContentType   string        `xml:"D:getcontenttype,omitempty"`
	GetETag          string        `xml:"D:getetag,omitempty"`
	GetLastModified  string        `xml:"D:getlastmodified,omitempty"`
	CreationDate     string        `xml:"D:creationdate,omitempty"`
	ResourceType     *resourceType `xml:"D:resourcetype"`
}

// resourceType indicates whether the resource is a collection (directory).
type resourceType struct {
	Collection *collection `xml:"D:collection,omitempty"`
}

// collection marks a resource as a directory
type collection struct {
	XMLName xml.Name `xml:"D:collection"`
}

// Lock-related XML structures

// lockResponse is the response to a LOCK request.
type lockResponse struct {
	XMLName       xml.Name      `xml:"D:prop"`
	XMLNS         string        `xml:"xmlns:D,attr"`
	LockDiscovery lockDiscovery `xml:"D:lockdiscovery"`
}

// lockDiscovery contains lock information.
type lockDiscovery struct {
	ActiveLock activeLock `xml:"D:activelock"`
}

// activeLock represents an active lock.
type activeLock struct {
	LockType  lockType     `xml:"D:locktype"`
	LockScope lockScope    `xml:"D:lockscope"`
	Depth     string       `xml:"D:depth"`
	Owner     owner        `xml:"D:owner"`
	Timeout   string       `xml:"D:timeout"`
	LockToken lockTokenXML `xml:"D:locktoken"`
	LockRoot  lockRoot     `xml:"D:lockroot"`
}

// lockType indicates the type of lock.
type lockType struct {
	Write *struct{} `xml:"D:write,omitempty"`
}

// lockScope indicates whether the lock is exclusive or shared.
type lockScope struct {
	Exclusive *struct{} `xml:"D:exclusive,omitempty"`
	Shared    *struct{} `xml:"D:shared,omitempty"`
}

// owner contains information about the lock owner.
type owner struct {
	Href string `xml:"D:href,omitempty"`
}

// lockTokenXML contains the lock token.
type lockTokenXML struct {
	Href string `xml:"D:href"`
}

// lockRoot contains the root of the locked resource.
type lockRoot struct {
	Href string `xml:"D:href"`
}

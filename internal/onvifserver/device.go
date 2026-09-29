package onvifserver

import (
	"fmt"
	"strings"
	"time"
)

// registerDeviceOps 注册 Device Service (tds) 操作。
func registerDeviceOps() {
	reg(nsDevice, "GetServices", opGetServices)
	reg(nsDevice, "GetServiceCapabilities", func(s *Service, _ []byte) (string, error) {
		return `<tds:GetServiceCapabilitiesResponse>` +
			`<tds:Capabilities Network="false" Security="false" System="false" IO="false"/>` +
			`</tds:GetServiceCapabilitiesResponse>`, nil
	})
	reg(nsDevice, "GetCapabilities", opGetCapabilities)
	reg(nsDevice, "GetDeviceInformation", opGetDeviceInformation)
	reg(nsDevice, "GetSystemDateAndTime", opGetSystemDateAndTime)
	reg(nsDevice, "GetHostname", func(s *Service, _ []byte) (string, error) {
		return `<tds:GetHostnameResponse><tt:Hostname>onvif-local.local</tt:Hostname></tds:GetHostnameResponse>`, nil
	})
	reg(nsDevice, "GetScopes", opGetScopes)
	reg(nsDevice, "GetNetworkInterfaces", func(s *Service, _ []byte) (string, error) {
		return `<tds:GetNetworkInterfacesResponse/>`, nil
	})
	reg(nsDevice, "GetNetworkDefaultGateway", func(s *Service, _ []byte) (string, error) {
		return `<tds:GetNetworkDefaultGatewayResponse/>`, nil
	})
	reg(nsDevice, "GetDNS", func(s *Service, _ []byte) (string, error) {
		return `<tds:GetDNSResponse><tt:FromDHCP>false</tt:FromDHCP><tt:SearchDomain>local</tt:SearchDomain></tds:GetDNSResponse>`, nil
	})
	reg(nsDevice, "GetNTP", func(s *Service, _ []byte) (string, error) {
		return `<tds:GetNTPResponse><tt:FromDHCP>false</tt:FromDHCP></tds:GetNTPResponse>`, nil
	})
	reg(nsDevice, "GetUsers", func(s *Service, _ []byte) (string, error) {
		return `<tds:GetUsersResponse><tds:User><tt:Username>admin</tt:Username><tt:UserLevel>Administrator</tt:UserLevel></tds:User></tds:GetUsersResponse>`, nil
	})
	reg(nsDevice, "GetRelayOutputs", func(s *Service, _ []byte) (string, error) {
		return `<tds:GetRelayOutputsResponse/>`, nil
	})
}

func serviceEntry(ns string) string {
	return fmt.Sprintf(`<tds:Service><tds:Namespace>%s</tds:Namespace>`, ns) +
		`<tds:XAddr>%XADDR%</tds:XAddr>` +
		`<tds:Version><tt:Major>2</tt:Major><tt:Minor>4</tt:Minor></tds:Version></tds:Service>`
}

func opGetServices(s *Service, _ []byte) (string, error) {
	xaddr := xmlEsc(s.uris.XAddr())
	body := `<tds:GetServicesResponse>` +
		strings.ReplaceAll(serviceEntry("http://www.onvif.org/ver10/device/wsdl"), "%XADDR%", xaddr) +
		strings.ReplaceAll(serviceEntry("http://www.onvif.org/ver10/media/wsdl"), "%XADDR%", xaddr) +
		strings.ReplaceAll(serviceEntry("http://www.onvif.org/ver20/ptz/wsdl"), "%XADDR%", xaddr) +
		strings.ReplaceAll(serviceEntry("http://www.onvif.org/ver10/imaging/wsdl"), "%XADDR%", xaddr) +
		`</tds:GetServicesResponse>`
	return body, nil
}

func opGetCapabilities(s *Service, _ []byte) (string, error) {
	x := xmlEsc(s.uris.XAddr())
	body := `<tds:GetCapabilitiesResponse><tds:Capabilities>` +
		`<tt:Device><tt:XAddr>` + x + `</tt:XAddr>` +
		`<tt:Network><tt:IPFilter>false</tt:IPFilter><tt:ZeroConfiguration>false</tt:ZeroConfiguration>` +
		`<tt:IPVersion6>false</tt:IPVersion6><tt:DynDNS>false</tt:DynDNS></tt:Network>` +
		`<tt:System><tt:DiscoveryResolve>false</tt:DiscoveryResolve><tt:DiscoveryBye>false</tt:DiscoveryBye>` +
		`<tt:RemoteDiscovery>false</tt:RemoteDiscovery><tt:SystemBackup>false</tt:SystemBackup>` +
		`<tt:SystemLogging>false</tt:SystemLogging><tt:FirmwareUpgrade>false</tt:FirmwareUpgrade></tt:System>` +
		`<tt:Security></tt:Security></tt:Device>` +
		`<tt:Media><tt:XAddr>` + x + `</tt:XAddr>` +
		`<tt:StreamingCapabilities><tt:RTPMulticast>false</tt:RTPMulticast><tt:RTP_TCP>true</tt:RTP_TCP>` +
		`<tt:RTP_RTSP_TCP>true</tt:RTP_RTSP_TCP><tt:NonAggregateControl>false</tt:NonAggregateControl>` +
		`<tt:NoRTSPStreaming>false</tt:NoRTSPStreaming></tt:StreamingCapabilities></tt:Media>` +
		`<tt:PTZ><tt:XAddr>` + x + `</tt:XAddr></tt:PTZ>` +
		`<tt:Imaging><tt:XAddr>` + x + `</tt:XAddr></tt:Imaging>` +
		`</tds:Capabilities></tds:GetCapabilitiesResponse>`
	return body, nil
}

func opGetDeviceInformation(s *Service, _ []byte) (string, error) {
	root := s.store.Root()
	body := `<tds:GetDeviceInformationResponse>` +
		`<tds:Manufacturer>` + xmlEsc(root.Manufacturer) + `</tds:Manufacturer>` +
		`<tds:Model>` + xmlEsc(root.Model) + `</tds:Model>` +
		`<tds:FirmwareVersion>` + xmlEsc(root.Firmware) + `</tds:FirmwareVersion>` +
		`<tds:SerialNumber>` + xmlEsc(root.Serial) + `</tds:SerialNumber>` +
		`<tds:HardwareId>onvif-local-v1</tds:HardwareId>` +
		`</tds:GetDeviceInformationResponse>`
	return body, nil
}

func opGetSystemDateAndTime(_ *Service, _ []byte) (string, error) {
	now := time.Now().UTC()
	dt := dateTimeXML(now)
	local := dateTimeXML(time.Now())
	body := `<tds:GetSystemDateAndTimeResponse><tds:SystemDateAndTime>` +
		`<tt:DateTimeType>Manual</tt:DateTimeType><tt:DaySavings>PT0S</tt:DaySavings><tt:TimeZone>UTC</tt:TimeZone>` +
		`<tt:UTCDateTime>` + dt + `</tt:UTCDateTime>` +
		`<tt:LocalDateTime>` + local + `</tt:LocalDateTime>` +
		`</tds:SystemDateAndTime></tds:GetSystemDateAndTimeResponse>`
	return body, nil
}

func dateTimeXML(t time.Time) string {
	return fmt.Sprintf(`<tt:Time><tt:Hour>%d</tt:Hour><tt:Minute>%d</tt:Minute><tt:Second>%d</tt:Second></tt:Time>`+
		`<tt:Date><tt:Year>%d</tt:Year><tt:Month>%d</tt:Month><tt:Day>%d</tt:Day></tt:Date>`,
		t.Hour(), t.Minute(), t.Second(), t.Year(), int(t.Month()), t.Day())
}

func opGetScopes(s *Service, _ []byte) (string, error) {
	root := s.store.Root()
	scopes := []string{
		"onvif://www.onvif.org/type/NetworkVideoTransmitter",
		"onvif://www.onvif.org/type/video_encoder",
		"onvif://www.onvif.org/type/ptz",
		"onvif://www.onvif.org/Profile/Streaming",
		"onvif://www.onvif.org/name/" + root.Model,
		"onvif://www.onvif.org/hardware/" + root.Model,
	}
	body := `<tds:GetScopesResponse>`
	for _, sc := range scopes {
		body += `<tds:Scopes><tt:ScopeDefinition>Fixed</tt:ScopeDefinition><tt:ScopeItem>` + xmlEsc(sc) + `</tt:ScopeItem></tds:Scopes>`
	}
	return body + `</tds:GetScopesResponse>`, nil
}

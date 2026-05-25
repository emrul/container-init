package systemd1shim

// managerIntrospectXML is the introspection blob the shim exposes at
// /org/freedesktop/systemd1. It lists exactly the methods and signals
// systemd-run (and Ptyxis's `--scope` wrapper) walk to validate that
// they're talking to the right thing before issuing StartTransientUnit.
//
// We don't claim to implement properties, the per-unit interface, or
// anything else real systemd exposes; clients that consult those will
// either fall back gracefully or fail loudly, which is the right
// failure mode for a stub.
const managerIntrospectXML = `<!DOCTYPE node PUBLIC "-//freedesktop//DTD D-BUS Object Introspection 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/introspect.dtd">
<node>
 <interface name="org.freedesktop.systemd1.Manager">
  <method name="StartTransientUnit">
   <arg name="name" type="s" direction="in"/>
   <arg name="mode" type="s" direction="in"/>
   <arg name="properties" type="a(sv)" direction="in"/>
   <arg name="aux" type="a(sa(sv))" direction="in"/>
   <arg name="job" type="o" direction="out"/>
  </method>
  <method name="StartUnit">
   <arg name="name" type="s" direction="in"/>
   <arg name="mode" type="s" direction="in"/>
   <arg name="job" type="o" direction="out"/>
  </method>
  <method name="StopUnit">
   <arg name="name" type="s" direction="in"/>
   <arg name="mode" type="s" direction="in"/>
   <arg name="job" type="o" direction="out"/>
  </method>
  <method name="ReloadUnit">
   <arg name="name" type="s" direction="in"/>
   <arg name="mode" type="s" direction="in"/>
   <arg name="job" type="o" direction="out"/>
  </method>
  <method name="RestartUnit">
   <arg name="name" type="s" direction="in"/>
   <arg name="mode" type="s" direction="in"/>
   <arg name="job" type="o" direction="out"/>
  </method>
  <method name="GetUnit">
   <arg name="name" type="s" direction="in"/>
   <arg name="unit" type="o" direction="out"/>
  </method>
  <method name="ListUnits">
   <arg name="units" type="a(ssssssouso)" direction="out"/>
  </method>
  <method name="Subscribe"/>
  <method name="Unsubscribe"/>
  <method name="Reload"/>
  <method name="Reexecute"/>
  <signal name="JobNew">
   <arg name="id" type="u"/>
   <arg name="job" type="o"/>
   <arg name="unit" type="s"/>
  </signal>
  <signal name="JobRemoved">
   <arg name="id" type="u"/>
   <arg name="job" type="o"/>
   <arg name="unit" type="s"/>
   <arg name="result" type="s"/>
  </signal>
 </interface>
 <interface name="org.freedesktop.DBus.Introspectable">
  <method name="Introspect">
   <arg name="data" type="s" direction="out"/>
  </method>
 </interface>
 <interface name="org.freedesktop.DBus.Peer">
  <method name="Ping"/>
  <method name="GetMachineId">
   <arg name="machine_uuid" type="s" direction="out"/>
  </method>
 </interface>
</node>
`

import CQNetworkIPC
import CQNetworkTransport
import Darwin
import Foundation
import NetworkExtension
import SystemExtensions

private func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data((message + "\n").utf8))
    exit(1)
}

private struct DescriptorLocation: Decodable { let path: String; let uid: uid_t }

private func reportStatus(extensionConnected: Bool, registered: Bool, brokerReady: Bool, counters: [String: UInt64] = [:]) -> Never {
    let ready = extensionConnected && registered && brokerReady
    var value: [String: Any] = ["ready": ready, "extension_connected": extensionConnected,
                              "broker_registered": registered, "broker_ready": brokerReady]
    value.merge(counters) { _, count in count }
    let data = try! JSONSerialization.data(withJSONObject: value, options: [.sortedKeys])
    FileHandle.standardOutput.write(data + Data([10]))
    exit(ready ? 0 : 1)
}

private final class Activation: NSObject, OSSystemExtensionRequestDelegate {
    let applicationPath: String
    init(applicationPath: String) { self.applicationPath = applicationPath }
    func request(_ request: OSSystemExtensionRequest, actionForReplacingExtension existing: OSSystemExtensionProperties,
                 withExtension ext: OSSystemExtensionProperties) -> OSSystemExtensionRequest.ReplacementAction { .replace }
    func requestNeedsUserApproval(_ request: OSSystemExtensionRequest) {
        FileHandle.standardError.write(Data("Approve CQ Network in System Settings to enable app routing.\n".utf8))
    }
    func request(_ request: OSSystemExtensionRequest, didFailWithError error: Error) {
        let failure = error as NSError
        let domain = failure.domain == OSSystemExtensionErrorDomain ? OSSystemExtensionErrorDomain + ", " : ""
        fail("CQ Network activation failed (\(domain)code \(failure.code)).")
    }
    func request(_ request: OSSystemExtensionRequest, didFinishWithResult result: OSSystemExtensionRequest.Result) {
        guard result == .completed else { fail("CQ Network activation will complete after a system restart.") }
        NETransparentProxyManager.loadAllFromPreferences { managers, error in
            guard error == nil else { fail("CQ Network configuration could not be read.") }
            let manager = managers?.first {
                ($0.protocolConfiguration as? NETunnelProviderProtocol)?.providerBundleIdentifier == CQNetworkIdentity.provider
            } ?? NETransparentProxyManager()
            let configuration = NETunnelProviderProtocol()
            configuration.providerBundleIdentifier = CQNetworkIdentity.provider
            configuration.serverAddress = "CQ app routing"
            configuration.providerConfiguration = ["application_path": self.applicationPath]
            manager.protocolConfiguration = configuration
            manager.localizedDescription = "CQ ChatGPT routing"
            manager.isEnabled = true
            manager.saveToPreferences { error in
                guard error == nil else { fail("CQ Network configuration could not be saved.") }
                manager.loadFromPreferences { error in
                    guard error == nil else { fail("CQ Network configuration could not be loaded.") }
                    do { try manager.connection.startVPNTunnel(); exit(0) }
                    catch { fail("CQ Network could not start.") }
                }
            }
        }
    }
}

let arguments = Array(CommandLine.arguments.dropFirst())
guard let command = arguments.first else { fail("Usage: cq-network run --cq PATH | activate --application PATH | status | disable") }
let machService: String
do { machService = try CQNetworkIdentity.machService() }
catch { fail("CQ Network requires a valid packaged build version.") }
func option(_ flag: String) -> String? {
    guard let index = arguments.firstIndex(of: flag), index + 1 < arguments.count else { return nil }
    return arguments[index + 1]
}
var retained: AnyObject?
switch command {
case "run":
    guard getuid() != 0, let executable = option("--cq"), executable.hasPrefix("/") else {
        fail("Run CQ Network as your user with an absolute --cq executable path.")
    }
    let process = Process()
    process.executableURL = URL(fileURLWithPath: executable)
    process.arguments = ["__native-proxy", "descriptor-path"]
    let output = Pipe()
    process.standardOutput = output
    process.standardError = FileHandle.nullDevice
    do { try process.run() } catch { fail("CQ Network could not locate its broker.") }
    let timeout = DispatchWorkItem { process.terminate() }
    DispatchQueue.global().asyncAfter(deadline: .now() + 10, execute: timeout)
    let data = output.fileHandleForReading.readData(ofLength: 8_193)
    process.waitUntilExit()
    timeout.cancel()
    guard data.count <= 8_192, process.terminationStatus == 0,
          let location = try? JSONDecoder().decode(DescriptorLocation.self, from: data),
          location.uid == getuid(), location.path.hasPrefix("/") else { fail("CQ Network broker descriptor is unavailable.") }
    let session = UserBrokerSession(connector: NativeBrokerConnector(descriptorURL: URL(fileURLWithPath: location.path), uid: getuid()))
    session.onReady = { ready in
        FileHandle.standardOutput.write(Data((ready ? "CQ network broker connected.\n" : "CQ network broker disconnected.\n").utf8))
    }
    retained = session
    session.start()
case "activate":
    guard let path = option("--application"), path.hasPrefix("/") else { fail("An absolute ChatGPT application path is required.") }
    NETransparentProxyManager.loadAllFromPreferences { managers, error in
        guard error == nil else { fail("CQ Network configuration could not be read.") }
        let existing = managers?.first {
            let configuration = $0.protocolConfiguration as? NETunnelProviderProtocol
            return configuration?.providerBundleIdentifier == CQNetworkIdentity.provider
                && configuration?.providerConfiguration?["application_path"] as? String == path
                && $0.isEnabled && $0.connection.status == .connected
        }
        if existing != nil {
            // Verify the already-running provider's signed identity. Do not
            // rewrite or restart a live configuration during repeated enable.
            let connection = NSXPCConnection(machServiceName: machService, options: .privileged)
            connection.setCodeSigningRequirement(CQNetworkIdentity.requirement(for: CQNetworkIdentity.provider))
            connection.remoteObjectInterface = NSXPCInterface(with: CQExtensionSession.self)
            retained = connection
            connection.activate()
            DispatchQueue.main.asyncAfter(deadline: .now() + 10) { fail("Existing CQ Network provider could not be verified.") }
            let proxy = connection.remoteObjectProxyWithErrorHandler { _ in
                fail("Existing CQ Network provider could not be verified.")
            } as? CQExtensionSession
            guard let proxy else { fail("Existing CQ Network provider could not be verified.") }
            proxy.status { _, _, _ in exit(0) }
            return
        }
        let activation = Activation(applicationPath: path)
        retained = activation
        let request = OSSystemExtensionRequest.activationRequest(forExtensionWithIdentifier: CQNetworkIdentity.provider, queue: .main)
        request.delegate = activation
        OSSystemExtensionManager.shared.submitRequest(request)
    }
case "status":
    DispatchQueue.main.asyncAfter(deadline: .now() + 10) {
        reportStatus(extensionConnected: false, registered: false, brokerReady: false)
    }
    NETransparentProxyManager.loadAllFromPreferences { managers, error in
        guard error == nil else { reportStatus(extensionConnected: false, registered: false, brokerReady: false) }
        let manager = managers?.first {
            ($0.protocolConfiguration as? NETunnelProviderProtocol)?.providerBundleIdentifier == CQNetworkIdentity.provider
        }
        guard manager?.isEnabled == true, manager?.connection.status == .connected else {
            reportStatus(extensionConnected: false, registered: false, brokerReady: false)
        }
        let connection = NSXPCConnection(machServiceName: machService, options: .privileged)
        connection.setCodeSigningRequirement(CQNetworkIdentity.requirement(for: CQNetworkIdentity.provider))
        connection.remoteObjectInterface = NSXPCInterface(with: CQExtensionSession.self)
        retained = connection
        connection.activate()
        let proxy = connection.remoteObjectProxyWithErrorHandler { _ in
            reportStatus(extensionConnected: true, registered: false, brokerReady: false)
        } as? CQExtensionSession
        guard let proxy else { reportStatus(extensionConnected: true, registered: false, brokerReady: false) }
        proxy.status { registered, ready, error in
            proxy.metrics { tcp, udp, opened, failed in
                reportStatus(extensionConnected: true, registered: registered, brokerReady: ready && error == nil,
                             counters: ["tcp_flows": tcp, "udp_flows": udp, "broker_opens": opened, "broker_failures": failed])
            }
        }
    }
case "disable":
    DispatchQueue.main.asyncAfter(deadline: .now() + 15) { fail("CQ Network disable timed out.") }
    NETransparentProxyManager.loadAllFromPreferences { managers, error in
        guard error == nil else { fail("CQ Network configuration could not be read.") }
        guard let manager = managers?.first(where: {
            ($0.protocolConfiguration as? NETunnelProviderProtocol)?.providerBundleIdentifier == CQNetworkIdentity.provider
        }) else { exit(0) }
        manager.connection.stopVPNTunnel()
        manager.isEnabled = false
        manager.saveToPreferences { error in
            guard error == nil else { fail("CQ Network configuration could not be disabled.") }
            exit(0)
        }
    }
default: fail("Unknown CQ Network command.")
}
RunLoop.main.run()

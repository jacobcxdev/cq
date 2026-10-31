import CQNetworkTransport
import Foundation
import NetworkExtension

@main
struct ProviderMain {
    static func main() {
        _ = CQTransparentProxyProvider.self
        NEProvider.startSystemExtensionMode()
        RunLoop.main.run()
    }
}

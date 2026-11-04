import CocoonBoxKit
import Testing

@Suite struct EndpointTests {
    @Test(arguments: ["http://localhost:50051", "http://127.0.0.1:50051", "http://[::1]:50051"])
    func aLoopbackURLMayUsePlaintext(url: String) throws {
        let endpoint = try #require(Endpoint(url: url))
        #expect(endpoint.isLoopback)
        #expect(endpoint.url == url)
    }

    @Test func aRemoteHostIsNotLoopback() throws {
        #expect(try #require(Endpoint(url: "http://box.example.com:50051")).isLoopback == false)
    }
}

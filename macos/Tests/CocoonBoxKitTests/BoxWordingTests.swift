import CocoonBoxKit
import Testing

@Suite struct BoxWordingTests {
    @Test func aPhoneHasPhoneWordsAndNoBrowserAndEveryOtherBoxIsADesktop() {
        let phone = BoxWording(.android)
        #expect(phone.noun == "phone" && phone.symbol == "iphone" && !phone.browser)
        #expect(!phone.saveHelp.contains("browser") && !phone.saveMessage.contains("browser"))
        for automation in [Cocoonbox_Agent_V1_Automation.desk, .unspecified, nil] {
            let desktop = BoxWording(automation)
            #expect(desktop.noun == "desktop" && desktop.symbol == "display" && desktop.browser, "\(String(describing: automation))")
        }
    }
}

import AppKit
import CocoonBoxKit
import SwiftUI

// PasswordSignInButton opens the sheet that fills the sign-in page of the box's browser.
struct PasswordSignInButton: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        Button {
            model.showPasswordSignIn = true
        } label: {
            Label("Sign in with Passwords", systemImage: "key.fill")
        }
        .help("Fill the sign-in page in the box with a password saved on this Mac")
    }
}

// PasswordSignInSheet holds the values only while it is open; the agent fills them
// into the active tab when its origin is still the one shown here.
struct PasswordSignInSheet: View {
    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @State private var origin: String?
    @State private var username = ""
    @State private var secret = ""
    @State private var oneTimeCode = false
    @State private var submit = true
    @State private var working = false
    @State private var error: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Sign in with Passwords").font(.title2.bold())
            Text(origin.map { "The box's browser is on \($0)." } ?? "Reading the box's browser…")
                .font(.callout)
            Picker("", selection: $oneTimeCode) {
                Text("Password").tag(false)
                Text("One-time code").tag(true)
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            if !oneTimeCode {
                LabeledContent("User name") {
                    CredentialField(text: $username, kind: .username)
                }
            }
            LabeledContent(oneTimeCode ? "Code" : "Password") {
                CredentialField(text: $secret, kind: oneTimeCode ? .oneTimeCode : .password)
                    .id(oneTimeCode)
            }
            Text("Paste from Passwords or 1Password, or type it. It goes to this page only and is never stored in the box.")
                .font(.callout)
                .foregroundStyle(.secondary)
            Toggle("Submit the form", isOn: $submit)
            if let error {
                Text(error).foregroundStyle(.red).font(.callout)
            }
            HStack {
                Spacer()
                Button("Cancel", role: .cancel) { dismiss() }
                    .keyboardShortcut(.cancelAction)
                Button("Fill") { fill() }
                    .keyboardShortcut(.defaultAction)
                    .disabled(origin == nil || secret.isEmpty || working)
            }
        }
        .padding(24)
        .frame(width: 460)
        .task {
            guard let connection = model.connection else { return }
            do {
                origin = try await connection.activeOrigin()
            } catch {
                self.error = BoxConnection.describe(error)
            }
        }
        .onChange(of: oneTimeCode) { secret = "" }
        .onDisappear {
            username = ""
            secret = ""
        }
    }

    private func fill() {
        guard let connection = model.connection, let origin else { return }
        let value: Cocoonbox_Agent_V1_FillCredentialRequest.OneOf_Secret = oneTimeCode ? .code(secret) : .password(secret)
        let user = oneTimeCode ? "" : username
        working = true
        error = nil
        Task {
            defer { working = false }
            do {
                _ = try await connection.fillCredential(origin: origin, username: user, secret: value, submit: submit)
                dismiss()
            } catch {
                self.error = BoxConnection.describe(error)
            }
        }
    }
}

// CredentialField is an AppKit field with a content type; an ad hoc signed app gets no AutoFill on it (design.md 20.4).
struct CredentialField: NSViewRepresentable {
    @Binding var text: String
    let kind: NSTextContentType

    func makeNSView(context: Context) -> NSTextField {
        let field = kind == .password ? NSSecureTextField() : NSTextField()
        field.contentType = kind
        field.delegate = context.coordinator
        field.stringValue = text
        return field
    }

    func updateNSView(_ field: NSTextField, context: Context) {
        if field.stringValue != text { field.stringValue = text }
    }

    func makeCoordinator() -> Coordinator {
        Coordinator(text: $text)
    }

    final class Coordinator: NSObject, NSTextFieldDelegate {
        let text: Binding<String>

        init(text: Binding<String>) {
            self.text = text
        }

        func controlTextDidChange(_ notification: Notification) {
            sync(notification)
        }

        func controlTextDidEndEditing(_ notification: Notification) {
            sync(notification)
        }

        private func sync(_ notification: Notification) {
            guard let field = notification.object as? NSTextField else { return }
            text.wrappedValue = field.stringValue
        }
    }
}

// The probe does nothing itself: it exists so that @vscode/test-electron has
// an extension in development to run the Production-mode specs from, while
// Drydock is an ordinary installed extension.
exports.activate = function activate() {}
exports.deactivate = function deactivate() {}

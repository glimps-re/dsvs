// Bodies are not wrapped: editors wrap them.
export default {
  extends: ["@commitlint/config-conventional"],
  // Dependabot writes "Bump" with a capital, and cannot be told otherwise.
  ignores: [(message) => message.includes("Signed-off-by: dependabot[bot]")],
  rules: {
    "body-max-line-length": [0],
    "footer-max-line-length": [0],
  },
};

import { test } from "node:test";
import assert from "node:assert/strict";
import { loginTarget } from "./api.js";

test("a 401 that names the login page sends the browser there", () => {
  assert.equal(loginTarget(401, { error: "sign in required", login: "/auth/login?next=%2F" }), "/auth/login?next=%2F");
});

test("anything else is an ordinary error", () => {
  assert.equal(loginTarget(401, "Unauthorized"), null);
  assert.equal(loginTarget(401, { error: "x" }), null);
  assert.equal(loginTarget(403, { login: "/auth/login" }), null);
  assert.equal(loginTarget(200, { login: "/auth/login" }), null);
});

test("only the app's own login page is followed", () => {
  assert.equal(loginTarget(401, { login: "https://evil.example/auth/login" }), null);
  assert.equal(loginTarget(401, { login: "//evil.example/auth/login" }), null);
});

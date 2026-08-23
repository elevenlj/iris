import test from "node:test";
import assert from "node:assert/strict";

import { SnakeGame } from "../internal/httpapi/static/snake-engine.mjs";

test("starts in a predictable ready state", () => {
  const game = new SnakeGame({ size: 10, random: () => 0 });
  assert.equal(game.status, "idle");
  assert.equal(game.snake.length, 3);
  assert.equal(game.score, 0);
  assert.equal(game.level, 1);
  assert.ok(!game.snake.some((part) => part.x === game.food.x && part.y === game.food.y));
});

test("moves, eats and increases difficulty", () => {
  const game = new SnakeGame({ size: 10, random: () => 0 });
  game.start();
  for (let score = 1; score <= 4; score += 1) {
    const head = game.snake[0];
    game.food = { x: head.x + 1, y: head.y };
    assert.equal(game.tick().type, "eat");
  }
  assert.equal(game.score, 4);
  assert.equal(game.level, 2);
  assert.equal(game.snake.length, 7);
  assert.ok(game.delay < 165);
});

test("rejects an immediate reverse direction", () => {
  const game = new SnakeGame({ size: 10 });
  game.start();
  assert.equal(game.setDirection("left"), false);
  game.tick();
  assert.equal(game.direction, "right");
});

test("detects wall and body collisions", () => {
  const wallGame = new SnakeGame({ size: 8 });
  wallGame.snake = [{ x: 7, y: 4 }, { x: 6, y: 4 }, { x: 5, y: 4 }];
  wallGame.start();
  assert.deepEqual(wallGame.tick(), { type: "gameover", reason: "wall" });

  const bodyGame = new SnakeGame({ size: 8 });
  bodyGame.snake = [
    { x: 4, y: 4 },
    { x: 4, y: 3 },
    { x: 3, y: 3 },
    { x: 3, y: 4 },
    { x: 3, y: 5 },
  ];
  bodyGame.direction = "up";
  bodyGame.pendingDirection = "up";
  bodyGame.food = { x: 0, y: 0 };
  bodyGame.start();
  bodyGame.setDirection("left");
  assert.deepEqual(bodyGame.tick(), { type: "gameover", reason: "body" });
});

test("pauses and resumes without advancing", () => {
  const game = new SnakeGame({ size: 10 });
  game.start();
  const head = { ...game.snake[0] };
  game.togglePause();
  assert.equal(game.tick().type, "idle");
  assert.deepEqual(game.snake[0], head);
  game.togglePause();
  assert.equal(game.status, "running");
});

-- Which products each dashboard user may see.
--
-- Enforced by the API for every request carrying X-Session-Token, admins
-- included (session.Scope): release management, per-product stats and the
-- machines list only show assigned products. A request with the admin key and
-- no session token (scripts, integrations) is not scoped.
--
-- Every existing user is assigned emly - until now it was the only product
-- anyone could see, and an empty assignment would lock the whole staff out of
-- the dashboard the moment this ships.
CREATE TABLE IF NOT EXISTS `user_products` (
    `user_id`    VARCHAR(255) NOT NULL,
    `product`    VARCHAR(20)  NOT NULL,
    `created_at` TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`user_id`, `product`),
    INDEX `idx_product` (`product`),
    FOREIGN KEY (`user_id`) REFERENCES `user` (`id`) ON DELETE CASCADE,
    FOREIGN KEY (`product`) REFERENCES `products` (`slug`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

INSERT IGNORE INTO `user_products` (`user_id`, `product`)
SELECT `id`, 'emly' FROM `user`;

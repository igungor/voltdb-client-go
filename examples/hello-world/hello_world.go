/* This file is part of VoltDB.
 * Copyright (C) 2008-2016 VoltDB Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with VoltDB.  If not, see <http://www.gnu.org/licenses/>.
 */
package main

import (
	"fmt"
	"github.com/VoltDB/voltdb-client-go/voltdbclient"
	"log"
)

func main() {
	client := voltdbclient.NewClient("", "")
	if err := client.CreateConnection("localhost:21212"); err != nil {
		log.Fatal("failed to connect to server")
	}
	defer func() {
		if client != nil {
			client.Close()
		}
	}()

	// rows to insert
	rows := make([][]string, 5)
	rows[0] = []string{"Hello", "World", "English"}
	rows[1] = []string{"Bonjour", "Monde", "French"}
	rows[2] = []string{"Hola", "Mundo", "Spanish"}
	rows[3] = []string{"Hej", "Verden", "Danish"}
	rows[4] = []string{"Ciao", "Mondo", "Italian"}
	for _, row := range rows {
		insertData(client, row[0], row[1], row[2])
	}
	response, err := client.Call("HELLOWORLD.select", "French")
	if err != nil {
		log.Fatal(err)
	}
	if response.TableCount() > 0 {
		table := response.Table(0)
		row, err := table.FetchRow(0)
		if err != nil {
			log.Fatal(err)
		}
		hello, nullHello, err := row.GetStringByName("HELLO")
		if err != nil {
			log.Fatal(err)
		}
		world, nullWorld, err := row.GetStringByName("WORLD")
		if err != nil {
			log.Fatal(err)
		}
		if nullHello || nullWorld {
			fmt.Println("Unexpected null values")
		} else {
			fmt.Printf("%v, %v!\n", hello, world)
		}
	} else {
		log.Fatal("Select statement didn't return any data")
	}
}

func insertData(client *voltdbclient.Client, hello, world, dialect string) {
	response, err := client.Call("HELLOWORLD.insert", hello, world, dialect)
	if err != nil {
		log.Fatal(err)
	}
	if response.Status() != voltdbclient.SUCCESS {
		log.Fatal("Insert failed with " + response.StatusString())
	}
}
